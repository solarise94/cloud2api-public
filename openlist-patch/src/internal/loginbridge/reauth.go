package loginbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// afterUpdateHook and afterRestoreHook, when set, run right after the failed
// UpdateStorage and right after the restore returns; tests use them to
// simulate an external edit before the decision that follows.
var afterUpdateHook, afterRestoreHook func(id uint)

// timeWindowSlack covers database timestamp rounding when deciding whether a
// row still carries our own write.
const timeWindowSlack = time.Millisecond

// reauthSecrets is the scrub list of a re-authorization: the new login's
// secrets plus the credential values still stored in the row. When the
// original credential is rejected during a restore, the driver error echoes
// the OLD token, which the new AuthConfig does not know.
func reauthSecrets(app loginsdk.App, auth *loginsdk.AuthConfig, orig *model.Storage) []string {
	var secrets []string
	if auth != nil {
		secrets = append(secrets, auth.Secrets...)
	}
	secrets = append(secrets, app.ClientSecret)
	if orig != nil {
		if addition, err := DecodeAddition(orig.Addition); err == nil {
			for k := range auth.Fields {
				if s, ok := addition[k].(string); ok {
					secrets = append(secrets, s)
				}
			}
		}
	}
	return secrets
}

// Reauth replaces the credential fields of an existing storage in place.
// Every other setting of the row is preserved, and when the new credentials
// fail to initialize, the original config is restored and verified.
func Reauth(ctx context.Context, a loginsdk.Adapter, app loginsdk.App, auth *loginsdk.AuthConfig, target loginsdk.Target) loginsdk.SaveResult {
	res := loginsdk.SaveResult{Mode: "reauth", StorageID: target.StorageID, MountPath: target.MountPath}
	if err := loginsdk.ValidateAuth(a, app, auth); err != nil {
		res.Message = "认证配置校验失败，未更新存储"
		res.Error = scrubMessage(err.Error(), app, auth)
		return res
	}
	m := a.Meta()
	unlock := lockStorage(target.StorageID)
	defer unlock()

	orig, err := db.GetStorageById(target.StorageID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			res.Message = fmt.Sprintf("存储 #%d 已被删除", target.StorageID)
		} else {
			res.Message = "读取存储失败"
			res.Error = scrubMessage(err.Error(), app, auth)
		}
		return res
	}
	res.MountPath = orig.MountPath
	// From here on the row's own credential values can surface in driver
	// errors, so they are scrubbed together with the new login's secrets.
	scrub := func(msg string) string { return loginsdk.Scrub(msg, reauthSecrets(app, auth, orig)...) }
	if orig.Driver != target.Driver || orig.Driver != m.Driver {
		res.Message = fmt.Sprintf("存储 #%d 的驱动 %s 与重新授权目标不一致，未写入新凭据", orig.ID, orig.Driver)
		return res
	}
	// Millisecond precision: the database column may round the timestamp on
	// its way out, but both sides were read through it.
	if orig.Modified.UnixMilli() != target.Modified.UnixMilli() {
		res.Message = "存储在授权期间被修改（例如在 OpenList 后台编辑），为避免覆盖，未写入新凭据；请重新开始重新授权"
		return res
	}
	if err := CheckTarget(orig, a); err != nil {
		res.Message = scrub(err.Error())
		return res
	}

	origAddition, err := DecodeAddition(orig.Addition)
	if err != nil {
		res.Message = "存储的附加配置无法解析，未更新"
		res.Error = scrub(err.Error())
		return res
	}
	// Only the credential fields are replaced; CreateFields are ignored here
	// and every other key (root, drive type, upload settings…) stays as is.
	newAddition := make(map[string]any, len(origAddition)+len(auth.Fields))
	for k, v := range origAddition {
		newAddition[k] = v
	}
	for k, v := range auth.Fields {
		newAddition[k] = v
	}
	newJSON, err := json.Marshal(newAddition)
	if err != nil {
		res.Message = "编码新的附加配置失败"
		res.Error = scrub(err.Error())
		return res
	}
	updated := *orig
	updated.Addition = string(newJSON)

	before := time.Now()
	updateErr := op.UpdateStorage(ctx, updated)
	after := time.Now()
	if afterUpdateHook != nil {
		afterUpdateHook(target.StorageID)
	}
	if updateErr == nil {
		res.OK = true
		res.CredentialsWritten = true
		if row, rerr := db.GetStorageById(target.StorageID); rerr == nil {
			res.Status = scrub(row.Status)
		}
		res.Message = fmt.Sprintf("已更新存储 #%d 的登录凭据；挂载点与其他设置保持不变", target.StorageID)
		log.Infof("connect: reauth storage #%d (mount %s, driver %s) saved", target.StorageID, orig.MountPath, m.Driver)
		return res
	}
	res.Error = scrub(updateErr.Error())

	cur, cerr := db.GetStorageById(target.StorageID)
	if cerr != nil {
		res.CredentialsWritten = true
		res.Message = fmt.Sprintf("新凭据初始化失败，且存储 #%d 已被删除", target.StorageID)
		log.Warnf("connect: reauth storage #%d (mount %s, driver %s) init failed, row deleted meanwhile", target.StorageID, orig.MountPath, m.Driver)
		return res
	}
	res.Status = scrub(cur.Status)
	curAddition, derr := DecodeAddition(cur.Addition)
	if derr != nil {
		curAddition = nil
	}
	if !rowCarriesWrite(cur, curAddition, origAddition, auth.Fields, before, after) {
		// The update failed before its database write; there is nothing to
		// undo and the original config is untouched.
		res.CredentialsWritten = false
		res.Message = "更新失败，原配置未改动"
		log.Warnf("connect: reauth storage #%d (mount %s, driver %s) failed before write", target.StorageID, orig.MountPath, m.Driver)
		return res
	}
	res.CredentialsWritten = true

	if cur.Modified.Before(before.Add(-timeWindowSlack)) || cur.Modified.After(after.Add(timeWindowSlack)) {
		// Someone changed the row after our failed write; restoring now would
		// overwrite their change.
		res.Restore = &loginsdk.RestoreReport{
			Attempted: false,
			Restored:  false,
			Error:     "存储在失败后又被其他操作修改，未自动恢复，请在 OpenList 中检查",
		}
		res.Message = fmt.Sprintf("新凭据初始化失败，且未能恢复原配置：存储 #%d 当前的配置可能包含新凭据（状态：%s），请在 OpenList 中检查", target.StorageID, res.Status)
		log.Warnf("connect: reauth storage #%d (mount %s, driver %s) init failed, row changed after failure", target.StorageID, orig.MountPath, m.Driver)
		return res
	}

	restoreErr := op.UpdateStorage(ctx, *orig)
	if afterRestoreHook != nil {
		afterRestoreHook(target.StorageID)
	}
	final, ferr := db.GetStorageById(target.StorageID)
	report := &loginsdk.RestoreReport{Attempted: true}
	if ferr == nil {
		credKeys := make(map[string]bool, len(auth.Fields))
		for k := range auth.Fields {
			credKeys[k] = true
		}
		finalAddition, _ := DecodeAddition(final.Addition)
		report.Status = scrub(final.Status)
		report.Restored = additionRestored(finalAddition, origAddition, credKeys, auth.Fields) && storageConfigEqual(final, orig)
		res.Status = report.Status
	}
	switch {
	case restoreErr != nil:
		report.Error = scrub(restoreErr.Error())
	case ferr != nil:
		report.Error = scrub(ferr.Error())
	case !report.Restored:
		report.Error = "恢复后校验未通过"
	}
	res.Restore = report
	switch {
	case report.Restored && report.Status == op.WORK:
		res.Message = "新凭据初始化失败，已恢复原配置（状态：work）"
	case report.Restored:
		res.Message = fmt.Sprintf("新凭据初始化失败，已恢复原配置，但原配置当前也无法初始化（状态：%s）", report.Status)
	default:
		res.Message = fmt.Sprintf("新凭据初始化失败，且未能恢复原配置：存储 #%d 当前的配置可能包含新凭据（状态：%s），请在 OpenList 中检查", target.StorageID, res.Status)
	}
	log.Warnf("connect: reauth storage #%d (mount %s, driver %s) init failed, restored=%v", target.StorageID, orig.MountPath, m.Driver, report.Restored)
	return res
}

// rowCarriesWrite reports whether the new credentials reached the row: a
// credential field holding a written value that differs from the original
// one, or a modification time inside our update's window (the driver may have
// rotated the value away during Init). A written value equal to the original
// (the same username, say) proves nothing either way.
func rowCarriesWrite(cur *model.Storage, curAddition, origAddition, written map[string]any, before, after time.Time) bool {
	if curAddition != nil {
		for k, v := range written {
			if ov, ok := origAddition[k]; ok && normalizeValue(ov) == normalizeValue(v) {
				continue
			}
			if cv, ok := curAddition[k]; ok && normalizeValue(cv) == normalizeValue(v) {
				return true
			}
		}
	}
	return !cur.Modified.Before(before.Add(-timeWindowSlack)) && !cur.Modified.After(after.Add(timeWindowSlack))
}

// normalizeValue makes values from different JSON decoders comparable:
// json.Number becomes int64 (or float64), other values stay as they are.
func normalizeValue(v any) any {
	if n, ok := v.(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			return i
		}
		f, _ := n.Float64()
		return f
	}
	return v
}

// additionRestored verifies that every non-credential addition key carries
// the original value again and every credential key holds its original value
// or one the driver rotated to — never a value only this save wrote.
func additionRestored(cur, orig map[string]any, credKeys map[string]bool, written map[string]any) bool {
	if cur == nil || orig == nil {
		return false
	}
	for k, v := range cur {
		if credKeys[k] {
			continue
		}
		ov, ok := orig[k]
		if !ok || normalizeValue(ov) != normalizeValue(v) {
			return false
		}
	}
	for k := range orig {
		if credKeys[k] {
			continue
		}
		if _, ok := cur[k]; !ok {
			return false
		}
	}
	for k, v := range written {
		cv, ok := cur[k]
		if !ok {
			continue
		}
		if ov, had := orig[k]; had && normalizeValue(ov) == normalizeValue(cv) {
			continue
		}
		if normalizeValue(cv) == normalizeValue(v) {
			return false
		}
	}
	return true
}

// storageConfigEqual compares every storage column a save must not touch.
// Modified and Status are excluded: the restore rewrites them by design.
func storageConfigEqual(a, b *model.Storage) bool {
	return a.MountPath == b.MountPath &&
		a.Order == b.Order &&
		a.Driver == b.Driver &&
		a.CacheExpiration == b.CacheExpiration &&
		a.CustomCachePolicies == b.CustomCachePolicies &&
		a.Remark == b.Remark &&
		a.Disabled == b.Disabled &&
		a.DisableIndex == b.DisableIndex &&
		a.EnableSign == b.EnableSign &&
		a.OrderBy == b.OrderBy &&
		a.OrderDirection == b.OrderDirection &&
		a.ExtractFolder == b.ExtractFolder &&
		a.WebProxy == b.WebProxy &&
		a.WebdavPolicy == b.WebdavPolicy &&
		a.ProxyRange == b.ProxyRange &&
		a.DownProxyURL == b.DownProxyURL &&
		a.DisableProxySign == b.DisableProxySign
}

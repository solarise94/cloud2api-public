// Package loginbridge is the config adaptation layer of the connect module:
// it writes a completed login (loginsdk.AuthConfig) into OpenList storages
// through OpenList's own storage operations — create with honest cleanup of a
// failed driver init, re-authorization in place with verified rollback — and
// persists the administrator's application profiles.
package loginbridge

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// CreateInput is the storage step of a creating session.
type CreateInput struct {
	MountPath string
	Root      string
	Remark    string
}

// beforeCreateHook, when set, runs after every check, right before the
// storage is created; afterCreateHook runs right after op.CreateStorage
// returns. Tests use them to occupy the mount path or edit the new row in
// between.
var (
	beforeCreateHook func(mount string)
	afterCreateHook  func(id uint)
)

const (
	maxMountBytes  = 256
	maxRootBytes   = 256
	maxRemarkRunes = 1000
)

func hasControlChars(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// scrubMessage removes every secret of the login and of the app profile from
// msg; driver errors routinely echo the token that was just rejected.
func scrubMessage(msg string, app loginsdk.App, auth *loginsdk.AuthConfig) string {
	var secrets []string
	if auth != nil {
		secrets = append(secrets, auth.Secrets...)
	}
	secrets = append(secrets, app.ClientSecret)
	return loginsdk.Scrub(msg, secrets...)
}

// cleanMountPath validates raw mount path input and returns the cleaned path.
func cleanMountPath(raw string) (mount string, problem string) {
	if hasControlChars(raw) {
		return "", "挂载路径包含控制字符"
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "挂载路径不能为空"
	}
	mount = utils.FixAndCleanPath(trimmed)
	switch {
	case mount == "/":
		return mount, "挂载路径不能是根目录 /，否则会遮蔽所有存储"
	case len(mount) > maxMountBytes:
		return mount, fmt.Sprintf("挂载路径过长（最多 %d 字节）", maxMountBytes)
	}
	return mount, ""
}

// cleanRoot validates the root value for the adapter's root field kind and
// returns the value to store.
func cleanRoot(rootField, root string) (string, bool) {
	if hasControlChars(root) {
		return "", false
	}
	if rootField == "root_folder_id" {
		if strings.Contains(root, "/") || len(root) > maxRootBytes {
			return "", false
		}
		return root, true
	}
	if !strings.HasPrefix(root, "/") {
		return "", false
	}
	cleaned := path.Clean(root)
	for _, seg := range strings.Split(cleaned, "/") {
		if seg == ".." {
			return "", false
		}
	}
	return cleaned, true
}

// Create writes a completed login into a new storage. Nothing is written
// until op.CreateStorage; when the driver init fails after the row was
// inserted, the row we inserted is removed again and the result says so.
func Create(ctx context.Context, a loginsdk.Adapter, app loginsdk.App, auth *loginsdk.AuthConfig, in CreateInput) loginsdk.SaveResult {
	res := loginsdk.SaveResult{Mode: "create"}
	m := a.Meta()
	if err := loginsdk.ValidateAuth(a, app, auth); err != nil {
		res.Message = "认证配置校验失败，未创建存储"
		res.Error = scrubMessage(err.Error(), app, auth)
		return res
	}

	mount, mountProblem := cleanMountPath(in.MountPath)
	res.MountPath = mount
	if mountProblem != "" {
		res.Retryable = true
		res.Message = mountProblem
		return res
	}
	if existing, err := db.GetStorageByMountPath(mount); err == nil {
		res.Retryable = true
		res.Message = fmt.Sprintf("挂载路径 %s 已被存储 #%d 使用", mount, existing.ID)
		return res
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		res.Message = "查询挂载路径失败"
		res.Error = scrubMessage(err.Error(), app, auth)
		return res
	}

	root := strings.TrimSpace(in.Root)
	if root == "" {
		root = m.RootDefault
	}
	root, rootOK := cleanRoot(m.RootField, root)
	if !rootOK {
		res.Retryable = true
		res.Message = "根目录的值无效：路径必须以 / 开头且不能包含 ..；ID 不能包含 /"
		return res
	}

	info, known := op.GetDriverInfoMap()[m.Driver]
	if !known {
		res.Message = fmt.Sprintf("驱动 %s 未注册，无法创建存储", m.Driver)
		return res
	}
	remark := strings.TrimSpace(in.Remark)
	if utf8.RuneCountInString(remark) > maxRemarkRunes {
		remark = string([]rune(remark)[:maxRemarkRunes])
	}
	addition, err := buildAddition(info.Additional, auth.CreateFields, auth.Fields, m.RootField, root)
	if err != nil {
		res.Message = "构建存储的附加配置失败"
		res.Error = scrubMessage(err.Error(), app, auth)
		return res
	}
	st, err := buildStorage(info, mount, remark)
	if err != nil {
		res.Message = "构建存储配置失败"
		res.Error = scrubMessage(err.Error(), app, auth)
		return res
	}
	st.Driver = m.Driver
	st.Addition = addition

	if beforeCreateHook != nil {
		beforeCreateHook(mount)
	}
	id, err := op.CreateStorage(ctx, *st)
	if afterCreateHook != nil {
		afterCreateHook(id)
	}
	res.StorageID = id
	if err == nil {
		res.OK = true
		res.CredentialsWritten = true
		if row, rerr := db.GetStorageById(id); rerr == nil {
			res.MountPath = row.MountPath
			res.Status = scrubMessage(row.Status, app, auth)
		}
		res.Message = fmt.Sprintf("已创建存储 #%d（%s），状态：%s", id, res.MountPath, res.Status)
		log.Infof("connect: created storage #%d (mount %s, driver %s), status: %s", id, res.MountPath, m.Driver, res.Status)
		return res
	}

	res.Error = scrubMessage(err.Error(), app, auth)
	if id == 0 {
		// CreateStorage failed before inserting anything; a row that now
		// occupies the mount path belongs to whoever won the race, not to us.
		res.Message = "保存失败，未创建存储：" + res.Error
		log.Warnf("connect: create storage %s (driver %s) failed before insert, nothing written", mount, m.Driver)
		return res
	}

	res.CredentialsWritten = true
	row, rerr := db.GetStorageById(id)
	if rerr != nil {
		// The failed row is already gone; nothing for us to clean up.
		res.Cleanup = &loginsdk.CleanupReport{StorageID: id, Deleted: true}
		res.Message = fmt.Sprintf("驱动初始化失败，本次创建的存储 #%d 已不存在", id)
		log.Warnf("connect: create storage #%d (mount %s, driver %s) init failed, row already gone", id, mount, m.Driver)
		return res
	}
	cleanup := &loginsdk.CleanupReport{StorageID: id}
	if row.MountPath == mount && row.Driver == m.Driver {
		cleanup.Attempted = true
		delErr := op.DeleteStorageById(ctx, id)
		// DeleteStorageById deletes the database row even when the driver's
		// Drop reports an error afterwards, so only a re-read decides.
		if _, aerr := db.GetStorageById(id); aerr != nil {
			cleanup.Deleted = true
		} else if delErr != nil {
			cleanup.Error = scrubMessage(delErr.Error(), app, auth)
		} else {
			cleanup.Error = "删除后存储仍存在"
		}
	} else {
		cleanup.Error = "数据库中的存储与本次创建不匹配，未自动删除"
	}
	res.Cleanup = cleanup
	if cleanup.Deleted {
		res.Message = fmt.Sprintf("驱动初始化失败，已删除本次创建的存储 #%d", id)
	} else {
		res.Message = fmt.Sprintf("驱动初始化失败，未能清理本次创建的存储 #%d，请在 OpenList 中手动删除该存储", id)
	}
	log.Warnf("connect: create storage #%d (mount %s, driver %s) init failed, cleanup deleted=%v", id, mount, m.Driver, cleanup.Deleted)
	return res
}

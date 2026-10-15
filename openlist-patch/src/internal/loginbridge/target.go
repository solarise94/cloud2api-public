package loginbridge

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
	"gorm.io/gorm"
)

// ReadTarget loads the storage a re-authorization session is pinned to.
func ReadTarget(id uint) (*model.Storage, error) {
	st, err := db.GetStorageById(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, loginsdk.Errorf(loginsdk.CodeNotFound, "存储 #%d 不存在", id)
		}
		return nil, loginsdk.Wrap(loginsdk.CodeInternal, "读取存储信息失败", err)
	}
	return st, nil
}

// TargetOf snapshots the storage identity a session compares again right
// before saving.
func TargetOf(st *model.Storage) loginsdk.Target {
	return loginsdk.Target{
		StorageID: st.ID,
		MountPath: st.MountPath,
		Driver:    st.Driver,
		Status:    st.Status,
		Modified:  st.Modified,
	}
}

// DecodeAddition decodes a storage addition JSON with numbers kept as
// json.Number so adapters can compare them precisely.
func DecodeAddition(s string) (map[string]any, error) {
	if strings.TrimSpace(s) == "" {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	out := map[string]any{}
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// CheckTarget verifies that storage st can be re-authorized by adapter a:
// same driver, not disabled, and accepted by the adapter's ReauthChecker
// when it has one.
func CheckTarget(st *model.Storage, a loginsdk.Adapter) error {
	m := a.Meta()
	if st.Driver != m.Driver {
		return loginsdk.Errorf(loginsdk.CodeConflict, "存储 #%d 使用驱动 %s，与平台的驱动 %s 不一致", st.ID, st.Driver, m.Driver)
	}
	if st.Disabled {
		return loginsdk.Errorf(loginsdk.CodeConflict, "存储已停用，请先在 OpenList 中启用后再重新授权")
	}
	if rc, ok := a.(loginsdk.ReauthChecker); ok {
		addition, err := DecodeAddition(st.Addition)
		if err != nil {
			return loginsdk.Wrap(loginsdk.CodeInternal, "存储的附加配置无法解析", err)
		}
		if err := rc.CheckReauth(addition); err != nil {
			return err
		}
	}
	return nil
}

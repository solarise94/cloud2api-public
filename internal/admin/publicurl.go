package admin

import (
	"errors"
	"net/url"
	"strconv"
	"strings"

	"cloud2api/internal/store"
)

// openlist_public_url handling (IMPLEMENTATION §地址配置): the address is the
// one an administrator's BROWSER uses to reach the OpenList admin UI. It is
// validated before it is ever persisted, and every manage/edit link is built
// by the helpers below — the frontend never concatenates URLs itself.

// NormalizeOpenlistPublicURL validates and normalizes an openlist_public_url
// value: the input is trimmed ("" means unset and passes), must be an absolute
// http/https address with a host, must carry no userinfo, query or fragment
// (a bare trailing "?" or "#" is rejected too) and must not already contain
// "/@manage"; trailing slashes are stripped and a path prefix is kept.
//
// The error message is Chinese (an administrator reads it in the portal). On
// error the returned string is the trimmed input, so display surfaces can echo
// what is stored alongside the error.
func NormalizeOpenlistPublicURL(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	bad := func(msg string) (string, error) {
		return strings.TrimRight(v, "/"), errors.New(msg)
	}
	// Everything after the first "?" or "#" would be a query or fragment, so
	// any occurrence of either character rejects the value — including a bare
	// trailing "?" or "#".
	if strings.Contains(v, "?") {
		return bad("地址不能包含查询参数（?…）")
	}
	if strings.Contains(v, "#") {
		return bad("地址不能包含锚点（#…）")
	}
	if strings.Contains(v, "/@manage") {
		return bad("地址不需要带 /@manage，填写 OpenList 后台根地址即可")
	}
	u, err := url.Parse(v)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return bad("不是合法的绝对地址：需要 http:// 或 https:// 开头并带主机名")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return bad("只支持 http:// 或 https:// 地址")
	}
	if u.User != nil {
		return bad("地址不能包含用户名或密码")
	}
	return strings.TrimRight(v, "/"), nil
}

// openlistManageURL is the ONE builder of the OpenList admin entry point
// (<base>/@manage); an empty base yields "".
func openlistManageURL(base string) string {
	if base == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + "/@manage"
}

// openlistStorageEditURL is the ONE builder of the deep link into the OpenList
// storage editor (<base>/@manage/storages/edit/<id>); empty when the base is
// unset/invalid or the storage id is unknown.
func openlistStorageEditURL(base string, storageID int64) string {
	manage := openlistManageURL(base)
	if manage == "" || storageID == 0 {
		return ""
	}
	return manage + "/storages/edit/" + strconv.FormatInt(storageID, 10)
}

// openlistConnectURL is the ONE builder of the patched OpenList's login page
// (<base>/@connect/, openlist-patch IMPLEMENTATION §适配补丁范围: cloud2api
// links to the page, it never handles credentials); an empty base yields "".
func openlistConnectURL(base string) string {
	if base == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + "/@connect/"
}

// openlistReauthURL is the ONE builder of the deep link re-authorizing storage
// N in the login module (<base>/@connect/#/reauth/<id>); empty when the base
// is unset or the storage id is unknown. Whether a storage may use it (the
// module is present and some platform re-authorizes its driver) is decided by
// the callers, not here.
func openlistReauthURL(base string, storageID int64) string {
	connect := openlistConnectURL(base)
	if connect == "" || storageID == 0 {
		return ""
	}
	return connect + "#/reauth/" + strconv.FormatInt(storageID, 10)
}

// openlistURLParts resolves a STORED openlist_public_url value into the
// normalized base, the manage URL and the validation error of that stored
// value (non-empty error → manage URL ""; the base echoes the trimmed input so
// the portal can show what is wrong).
func openlistURLParts(stored string) (publicURL, manageURL, publicURLError string) {
	base, err := NormalizeOpenlistPublicURL(stored)
	if err != nil {
		return base, "", err.Error()
	}
	return base, openlistManageURL(base), ""
}

// openlistBaseURL returns the validated openlist_public_url base ("" when
// unset or when the stored value is invalid — links are then omitted).
func (p *Portal) openlistBaseURL() string {
	v, _ := p.st.GetSetting("openlist_public_url")
	base, err := NormalizeOpenlistPublicURL(v)
	if err != nil {
		return ""
	}
	return base
}

// SeedOpenlistPublicURL persists a boot-provided openlist_public_url value
// (env C2A_OPENLIST_PUBLIC_URL in main). A blank value seeds nothing; an
// invalid value is reported to the caller and the stored value is kept. The
// normalized value is returned so the caller can log it.
func SeedOpenlistPublicURL(st *store.Store, raw string) (string, error) {
	base, err := NormalizeOpenlistPublicURL(raw)
	if err != nil {
		return "", err
	}
	if base == "" {
		return "", nil
	}
	if err := st.SetSetting("openlist_public_url", base); err != nil {
		return "", err
	}
	return base, nil
}

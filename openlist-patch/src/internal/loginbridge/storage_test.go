package loginbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

func assertGone(t *testing.T, id uint) {
	t.Helper()
	if _, err := db.GetStorageById(id); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("storage #%d still in db: %v", id, err)
	}
}

func assertNoSecret(t *testing.T, label, secret string, fields ...string) {
	t.Helper()
	for _, f := range fields {
		if secret != "" && strings.Contains(f, secret) {
			t.Errorf("%s leaks secret %q in %q", label, secret, f)
		}
	}
}

func resultTexts(res loginsdk.SaveResult) []string {
	texts := []string{res.Message, res.Error, res.Status}
	if res.Cleanup != nil {
		texts = append(texts, res.Cleanup.Error)
	}
	if res.Restore != nil {
		texts = append(texts, res.Restore.Error, res.Restore.Status)
	}
	return texts
}

func TestDecodeAddition(t *testing.T) {
	m, err := DecodeAddition("")
	if err != nil || len(m) != 0 {
		t.Fatalf("empty addition: %v %v", m, err)
	}
	m, err = DecodeAddition(`{"a":1,"b":1.5,"c":"x","d":true}`)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m["a"] != json.Number("1") || m["b"] != json.Number("1.5") || m["c"] != "x" || m["d"] != true {
		t.Fatalf("decoded %v", m)
	}
	if _, err := DecodeAddition("{"); err == nil {
		t.Fatal("invalid json accepted")
	}
}

func TestCreateSuccess(t *testing.T) {
	adapter := newFakeAdapter()
	auth := fakeAuth("good", map[string]any{"use_online_api": false})
	auth.CreateFields = map[string]any{"keep": "kept-value"}
	res := Create(context.Background(), adapter, fakeApp(), auth, CreateInput{
		MountPath: "/t-create-ok",
		Remark:    "  备注一  ",
	})
	if !res.OK || res.Mode != "create" || !res.CredentialsWritten || res.Retryable {
		t.Fatalf("create result: %+v", res)
	}
	if res.StorageID == 0 {
		t.Fatal("no storage id")
	}
	if res.Status != op.WORK {
		t.Fatalf("status %q, want work", res.Status)
	}
	if !strings.Contains(res.Message, "已创建存储 #") {
		t.Fatalf("message %q", res.Message)
	}

	row := mustRow(t, res.StorageID)
	if row.Driver != "ConnectFake" || row.MountPath != "/t-create-ok" {
		t.Fatalf("row %+v", row)
	}
	if row.Remark != "备注一" || row.Order != 0 {
		t.Fatalf("remark %q order %d", row.Remark, row.Order)
	}
	// Common form defaults, the way the native form would submit them.
	if row.CacheExpiration != 30 || row.WebdavPolicy != "302_redirect" || row.WebProxy {
		t.Fatalf("common defaults wrong: %+v", row.Proxy)
	}

	addition := mustDecode(t, row.Addition)
	// Typed driver defaults.
	if addition["upload_thread"] != "3" {
		t.Fatalf("upload_thread %v", addition["upload_thread"])
	}
	if addition["chunk_size"] != json.Number("5") {
		t.Fatalf("chunk_size %v (%T)", addition["chunk_size"], addition["chunk_size"])
	}
	if addition["mode"] != "b" {
		t.Fatalf("mode %v", addition["mode"])
	}
	if addition["use_online_api"] != false {
		t.Fatalf("auth field override failed: use_online_api %v", addition["use_online_api"])
	}
	if addition["refresh_token"] != "good" {
		t.Fatalf("refresh_token %v", addition["refresh_token"])
	}
	// Create-only fields are applied on create.
	if addition["keep"] != "kept-value" {
		t.Fatalf("create field keep %v", addition["keep"])
	}
	// Root default.
	if addition["root_folder_path"] != "/" {
		t.Fatalf("root default %v", addition["root_folder_path"])
	}
	if _, err := op.GetStorageByMountPath("/t-create-ok"); err != nil {
		t.Fatalf("storage not in memory: %v", err)
	}

	// Explicit root is cleaned.
	res2 := Create(context.Background(), adapter, fakeApp(), fakeAuth("good", nil), CreateInput{
		MountPath: "/t-create-root",
		Root:      "/a/b/../c",
	})
	if !res2.OK {
		t.Fatalf("create with root: %+v", res2)
	}
	addition2 := mustDecode(t, mustRow(t, res2.StorageID).Addition)
	if addition2["root_folder_path"] != "/a/c" {
		t.Fatalf("cleaned root %v", addition2["root_folder_path"])
	}
}

func TestCreateRetryableInput(t *testing.T) {
	adapter := newFakeAdapter()
	ctx := context.Background()

	occupied := Create(ctx, adapter, fakeApp(), fakeAuth("good", nil), CreateInput{MountPath: "/t-taken"})
	if !occupied.OK {
		t.Fatalf("setup create: %+v", occupied)
	}

	cases := []struct {
		name string
		in   CreateInput
		want string
	}{
		{"mount taken", CreateInput{MountPath: "/t-taken"}, "已被存储 #"},
		{"mount root", CreateInput{MountPath: "/"}, ""},
		{"mount empty", CreateInput{MountPath: "   "}, ""},
		{"mount control char", CreateInput{MountPath: "/a\nb"}, ""},
		{"mount too long", CreateInput{MountPath: "/" + strings.Repeat("a", 300)}, ""},
		{"bad root path", CreateInput{MountPath: "/t-badroot", Root: "../x"}, ""},
		{"root without slash", CreateInput{MountPath: "/t-badroot", Root: "x"}, ""},
		{"root control char", CreateInput{MountPath: "/t-badroot", Root: "/\x01"}, ""},
	}
	for _, c := range cases {
		res := Create(ctx, adapter, fakeApp(), fakeAuth("good", nil), c.in)
		if res.OK || !res.Retryable || res.CredentialsWritten {
			t.Fatalf("%s: %+v", c.name, res)
		}
		if c.want != "" && !strings.Contains(res.Message, c.want) {
			t.Fatalf("%s: message %q, want %q", c.name, res.Message, c.want)
		}
	}
	// ID-type root: a slash is invalid there too.
	idAdapter := &fakeAdapter{rootField: "root_folder_id"}
	res := Create(ctx, idAdapter, fakeApp(), fakeAuth("good", nil), CreateInput{MountPath: "/t-badroot-id", Root: "a/b"})
	if res.OK || !res.Retryable || res.CredentialsWritten {
		t.Fatalf("id root: %+v", res)
	}

	// Nothing was written: the occupied storage is untouched, the rejected
	// mounts are free.
	row := mustRow(t, occupied.StorageID)
	if mustDecode(t, row.Addition)["refresh_token"] != "good" {
		t.Fatalf("occupied storage changed: %+v", row)
	}
	for _, mount := range []string{"/", "/t-badroot", "/t-badroot-id"} {
		if _, err := db.GetStorageByMountPath(mount); err == nil {
			t.Fatalf("mount %s was written", mount)
		}
	}
}

func TestCreateValidateAuthFailure(t *testing.T) {
	adapter := newFakeAdapter()
	auth := fakeAuth("good", nil)
	auth.Fields["undeclared_field"] = "x"
	res := Create(context.Background(), adapter, fakeApp(), auth, CreateInput{MountPath: "/t-invalid-auth"})
	if res.OK || res.Retryable || res.CredentialsWritten {
		t.Fatalf("result: %+v", res)
	}
	if _, err := db.GetStorageByMountPath("/t-invalid-auth"); err == nil {
		t.Fatal("storage written despite invalid auth config")
	}
}

func TestCreateInitFailureCleanup(t *testing.T) {
	adapter := newFakeAdapter()
	token := "bad-secret-token-1234"
	res := Create(context.Background(), adapter, fakeApp(), fakeAuth(token, nil), CreateInput{MountPath: "/t-init-fail"})
	if res.OK || res.Retryable {
		t.Fatalf("result: %+v", res)
	}
	if res.StorageID == 0 {
		t.Fatal("no storage id reported")
	}
	if !res.CredentialsWritten {
		t.Fatal("credentials were written to the row, result must say so")
	}
	if res.Cleanup == nil || !res.Cleanup.Attempted || !res.Cleanup.Deleted || res.Cleanup.StorageID != res.StorageID {
		t.Fatalf("cleanup report: %+v", res.Cleanup)
	}
	if !strings.Contains(res.Message, "已删除本次创建的存储") {
		t.Fatalf("message %q", res.Message)
	}
	assertNoSecret(t, "create init failure", token, resultTexts(res)...)
	assertGone(t, res.StorageID)
	if _, err := op.GetStorageByMountPath("/t-init-fail"); err == nil {
		t.Fatal("driver still in memory after cleanup")
	}
	if _, err := db.GetStorageByMountPath("/t-init-fail"); err == nil {
		t.Fatal("row still in db after cleanup")
	}
}

func TestCreateInsertRaceFailsClean(t *testing.T) {
	adapter := newFakeAdapter()
	ctx := context.Background()
	keep := Create(ctx, adapter, fakeApp(), fakeAuth("good", nil), CreateInput{MountPath: "/t-id0-keep"})
	if !keep.OK {
		t.Fatalf("setup: %+v", keep)
	}

	// A concurrent request wins the mount path between our check and the
	// insert; op.CreateStorage fails with id 0.
	beforeCreateHook = func(mount string) {
		if err := db.CreateStorage(&model.Storage{
			Driver:    "ConnectFake",
			MountPath: mount,
			Addition:  `{"refresh_token":"good","root_folder_path":"/"}`,
		}); err != nil {
			t.Errorf("hook insert: %v", err)
		}
	}
	defer func() { beforeCreateHook = nil }()

	res := Create(ctx, adapter, fakeApp(), fakeAuth("good", nil), CreateInput{MountPath: "/t-id0-race"})
	if res.OK || res.Retryable {
		t.Fatalf("result: %+v", res)
	}
	if res.CredentialsWritten || res.Cleanup != nil {
		t.Fatalf("nothing was ours to clean: %+v", res)
	}
	if !strings.Contains(res.Message, "保存失败，未创建存储") {
		t.Fatalf("message %q", res.Message)
	}
	if res.StorageID != 0 {
		t.Fatalf("storage id %d, want 0", res.StorageID)
	}
	// The other request's row at the mount path is not ours to delete.
	foreign, err := db.GetStorageByMountPath("/t-id0-race")
	if err != nil {
		t.Fatalf("foreign row deleted: %v", err)
	}
	if mustDecode(t, foreign.Addition)["refresh_token"] != "good" {
		t.Fatalf("foreign row changed: %+v", foreign)
	}
	mustRow(t, keep.StorageID)
}

func TestReauthSuccess(t *testing.T) {
	adapter := newFakeAdapter()
	ctx := context.Background()
	created := Create(ctx, adapter, fakeApp(), fakeAuth("good", nil), CreateInput{
		MountPath: "/t-reauth-ok",
		Remark:    "备注二",
	})
	if !created.OK {
		t.Fatalf("setup: %+v", created)
	}
	before := *mustRow(t, created.StorageID)
	target := TargetOf(&before)

	res := Reauth(ctx, adapter, fakeApp(), fakeAuth("good2", nil), target)
	if !res.OK || res.Mode != "reauth" || !res.CredentialsWritten || res.Retryable {
		t.Fatalf("result: %+v", res)
	}
	if res.Status != op.WORK {
		t.Fatalf("status %q", res.Status)
	}
	if !strings.Contains(res.Message, "已更新存储 #") || !strings.Contains(res.Message, "挂载点与其他设置保持不变") {
		t.Fatalf("message %q", res.Message)
	}

	after := mustRow(t, created.StorageID)
	addition := mustDecode(t, after.Addition)
	if addition["refresh_token"] != "good2" {
		t.Fatalf("refresh_token %v", addition["refresh_token"])
	}
	// Every non-credential setting is preserved.
	if addition["root_folder_path"] != "/" || addition["upload_thread"] != "3" ||
		addition["mode"] != "b" || addition["keep"] != "" || addition["use_online_api"] != true {
		t.Fatalf("non-credential addition changed: %v", addition)
	}
	if !storageConfigEqual(after, &before) {
		t.Fatalf("storage config changed:\nbefore %+v\nafter  %+v", before, after)
	}
}

func TestReauthRefusedBeforeWriting(t *testing.T) {
	adapter := newFakeAdapter()
	ctx := context.Background()
	newAuth := func() *loginsdk.AuthConfig { return fakeAuth("good-new", nil) }

	// Storage deleted since the session started.
	deleted := Create(ctx, adapter, fakeApp(), fakeAuth("good", nil), CreateInput{MountPath: "/t-ref-deleted"})
	row := mustRow(t, deleted.StorageID)
	target := TargetOf(row)
	if err := op.DeleteStorageById(ctx, deleted.StorageID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	res := Reauth(ctx, adapter, fakeApp(), newAuth(), target)
	if res.OK || res.CredentialsWritten || !strings.Contains(res.Message, "已被删除") {
		t.Fatalf("deleted: %+v", res)
	}

	// Driver mismatch with the target.
	created := Create(ctx, adapter, fakeApp(), fakeAuth("good", nil), CreateInput{MountPath: "/t-ref-driver"})
	row = mustRow(t, created.StorageID)
	target = TargetOf(row)
	target.Driver = "OtherDriver"
	snapshot := *row
	res = Reauth(ctx, adapter, fakeApp(), newAuth(), target)
	if res.OK || res.CredentialsWritten || !strings.Contains(res.Message, "驱动") {
		t.Fatalf("driver mismatch: %+v", res)
	}
	if !reflect.DeepEqual(snapshot, *mustRow(t, created.StorageID)) {
		t.Fatal("row changed on driver mismatch refusal")
	}

	// Modified since the session started (a native edit).
	target = TargetOf(mustRow(t, created.StorageID))
	time.Sleep(5 * time.Millisecond)
	edited := *mustRow(t, created.StorageID)
	edited.Remark = "后台修改"
	if err := op.UpdateStorage(ctx, edited); err != nil {
		t.Fatalf("native edit: %v", err)
	}
	snapshot = *mustRow(t, created.StorageID)
	res = Reauth(ctx, adapter, fakeApp(), newAuth(), target)
	if res.OK || res.CredentialsWritten || res.Retryable {
		t.Fatalf("modified: %+v", res)
	}
	if !strings.Contains(res.Message, "存储在授权期间被修改") {
		t.Fatalf("message %q", res.Message)
	}
	if !reflect.DeepEqual(snapshot, *mustRow(t, created.StorageID)) {
		t.Fatal("row changed on modification conflict refusal")
	}

	// Disabled storage.
	if err := op.DisableStorage(ctx, created.StorageID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	snapshot = *mustRow(t, created.StorageID)
	target = TargetOf(&snapshot)
	res = Reauth(ctx, adapter, fakeApp(), newAuth(), target)
	if res.OK || res.CredentialsWritten || !strings.Contains(res.Message, "存储已停用") {
		t.Fatalf("disabled: %+v", res)
	}
	if !reflect.DeepEqual(snapshot, *mustRow(t, created.StorageID)) {
		t.Fatal("row changed on disabled refusal")
	}

	// The adapter's ReauthChecker rejects this storage.
	pickyStorage := Create(ctx, adapter, fakeApp(), fakeAuth("good", nil), CreateInput{MountPath: "/t-ref-picky"})
	if !pickyStorage.OK {
		t.Fatalf("picky setup: %+v", pickyStorage)
	}
	pickyRow := mustRow(t, pickyStorage.StorageID)
	pickyTarget := TargetOf(pickyRow)
	pickySnapshot := *pickyRow
	picky := &fakeAdapter{reauthErr: loginsdk.Errorf(loginsdk.CodeUnsupported, "不支持该地区")}
	res = Reauth(ctx, picky, fakeApp(), newAuth(), pickyTarget)
	if res.OK || res.CredentialsWritten || !strings.Contains(res.Message, "不支持该地区") {
		t.Fatalf("reauth checker: %+v", res)
	}
	if !reflect.DeepEqual(pickySnapshot, *mustRow(t, pickyStorage.StorageID)) {
		t.Fatal("row changed on reauth checker refusal")
	}
}

func TestReauthRestoreWorkingOriginal(t *testing.T) {
	adapter := newFakeAdapter()
	ctx := context.Background()
	created := Create(ctx, adapter, fakeApp(), fakeAuth("good", nil), CreateInput{MountPath: "/t-restore-ok"})
	if !created.OK {
		t.Fatalf("setup: %+v", created)
	}
	before := *mustRow(t, created.StorageID)
	target := TargetOf(&before)

	token := "bad-new-cred-9876"
	res := Reauth(ctx, adapter, fakeApp(), fakeAuth(token, nil), target)
	if res.OK || res.Retryable || !res.CredentialsWritten {
		t.Fatalf("result: %+v", res)
	}
	if res.Restore == nil || !res.Restore.Attempted || !res.Restore.Restored {
		t.Fatalf("restore report: %+v", res.Restore)
	}
	if res.Restore.Status != op.WORK {
		t.Fatalf("restore status %q", res.Restore.Status)
	}
	if !strings.Contains(res.Message, "已恢复原配置（状态：work）") {
		t.Fatalf("message %q", res.Message)
	}
	assertNoSecret(t, "reauth restore", token, resultTexts(res)...)

	after := mustRow(t, created.StorageID)
	addition := mustDecode(t, after.Addition)
	if addition["refresh_token"] != "good" {
		t.Fatalf("original credential not restored: %v", addition["refresh_token"])
	}
	if after.Status != op.WORK {
		t.Fatalf("status after restore %q", after.Status)
	}
	if !storageConfigEqual(after, &before) {
		t.Fatalf("config not restored:\nbefore %+v\nafter  %+v", before, after)
	}
}

func TestReauthRestoreOriginalAlsoFails(t *testing.T) {
	adapter := newFakeAdapter()
	ctx := context.Background()
	// A storage whose original credential is already broken — the usual
	// reason to re-authorize. op.CreateStorage keeps the row on init error.
	origToken := "bad-orig-cred-5555"
	id, err := op.CreateStorage(ctx, model.Storage{
		Driver:    "ConnectFake",
		MountPath: "/t-restore-origbad",
		Addition:  fmt.Sprintf(`{"refresh_token":%q,"root_folder_path":"/"}`, origToken),
	})
	if err == nil {
		t.Fatal("expected the setup create to fail its init")
	}
	if id == 0 {
		t.Fatal("the failed create kept no row")
	}
	target := TargetOf(mustRow(t, id))

	newToken := "bad-new-cred-6666"
	res := Reauth(ctx, adapter, fakeApp(), fakeAuth(newToken, nil), target)
	if res.OK || !res.CredentialsWritten {
		t.Fatalf("result: %+v", res)
	}
	if res.Restore == nil || !res.Restore.Attempted || !res.Restore.Restored {
		t.Fatalf("restore report: %+v", res.Restore)
	}
	if res.Restore.Status == op.WORK {
		t.Fatalf("original init was supposed to fail too, status %q", res.Restore.Status)
	}
	if !strings.Contains(res.Message, "已恢复原配置，但原配置当前也无法初始化") {
		t.Fatalf("message %q", res.Message)
	}
	assertNoSecret(t, "reauth origbad", origToken, resultTexts(res)...)
	assertNoSecret(t, "reauth origbad new", newToken, resultTexts(res)...)

	row := mustRow(t, id)
	if mustDecode(t, row.Addition)["refresh_token"] != origToken {
		t.Fatalf("original credential not restored: %+v", row)
	}
	if row.Status == op.WORK {
		t.Fatalf("row status %q", row.Status)
	}
}

func TestReauthRotateThenFailStillRestores(t *testing.T) {
	adapter := newFakeAdapter()
	ctx := context.Background()
	created := Create(ctx, adapter, fakeApp(), fakeAuth("good", nil), CreateInput{MountPath: "/t-rotbad"})
	if !created.OK {
		t.Fatalf("setup: %+v", created)
	}
	target := TargetOf(mustRow(t, created.StorageID))

	// The driver rotates the token and persists it before failing.
	res := Reauth(ctx, adapter, fakeApp(), fakeAuth("rotate-bad", nil), target)
	if res.OK || !res.CredentialsWritten {
		t.Fatalf("result: %+v", res)
	}
	if res.Restore == nil || !res.Restore.Restored || res.Restore.Status != op.WORK {
		t.Fatalf("restore report: %+v", res.Restore)
	}
	row := mustRow(t, created.StorageID)
	if got := mustDecode(t, row.Addition)["refresh_token"]; got != "good" {
		t.Fatalf("refresh_token %v, want the restored original", got)
	}
}

func TestReauthExternalEditAfterFailureNoRestore(t *testing.T) {
	adapter := newFakeAdapter()
	ctx := context.Background()
	created := Create(ctx, adapter, fakeApp(), fakeAuth("good", nil), CreateInput{MountPath: "/t-extedit"})
	if !created.OK {
		t.Fatalf("setup: %+v", created)
	}
	target := TargetOf(mustRow(t, created.StorageID))

	token := "bad-ext-edit-7777"
	afterUpdateHook = func(id uint) {
		st, err := db.GetStorageById(id)
		if err != nil {
			t.Errorf("hook read: %v", err)
			return
		}
		st.Modified = time.Now().Add(time.Hour)
		st.Remark = "外部修改"
		if err := db.UpdateStorage(st); err != nil {
			t.Errorf("hook edit: %v", err)
		}
	}
	defer func() { afterUpdateHook = nil }()

	res := Reauth(ctx, adapter, fakeApp(), fakeAuth(token, nil), target)
	if res.OK || !res.CredentialsWritten {
		t.Fatalf("result: %+v", res)
	}
	if res.Restore == nil || res.Restore.Attempted || res.Restore.Restored {
		t.Fatalf("restore must not be attempted: %+v", res.Restore)
	}
	if !strings.Contains(res.Restore.Error, "未自动恢复") {
		t.Fatalf("restore error %q", res.Restore.Error)
	}
	if !strings.Contains(res.Message, "请在 OpenList 中检查") {
		t.Fatalf("message %q", res.Message)
	}
	assertNoSecret(t, "reauth external edit", token, resultTexts(res)...)

	// The row still carries the new credentials; we must not have claimed a
	// successful restore.
	row := mustRow(t, created.StorageID)
	if got := mustDecode(t, row.Addition)["refresh_token"]; got != token {
		t.Fatalf("refresh_token %v, want the written credential untouched", got)
	}
}

func TestReauthConcurrentSameTarget(t *testing.T) {
	adapter := newFakeAdapter()
	ctx := context.Background()
	created := Create(ctx, adapter, fakeApp(), fakeAuth("good", nil), CreateInput{MountPath: "/t-race"})
	if !created.OK {
		t.Fatalf("setup: %+v", created)
	}
	target := TargetOf(mustRow(t, created.StorageID))
	time.Sleep(5 * time.Millisecond) // our update must land in a later millisecond

	var wg sync.WaitGroup
	results := make([]loginsdk.SaveResult, 2)
	start := make(chan struct{})
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = Reauth(ctx, adapter, fakeApp(), fakeAuth(fmt.Sprintf("good-race-%d", i), nil), target)
		}(i)
	}
	close(start)
	wg.Wait()

	ok := 0
	for i, res := range results {
		if res.OK {
			ok++
			continue
		}
		if res.CredentialsWritten || !strings.Contains(res.Message, "存储在授权期间被修改") {
			t.Fatalf("loser %d: %+v", i, res)
		}
	}
	if ok != 1 {
		t.Fatalf("exactly one reauth must succeed, got %d: %+v", ok, results)
	}
	// The winner's credential is in the row.
	got := mustDecode(t, mustRow(t, created.StorageID).Addition)["refresh_token"]
	if got != "good-race-0" && got != "good-race-1" {
		t.Fatalf("row refresh_token %v", got)
	}
}

func TestRowCarriesWriteDetection(t *testing.T) {
	written := map[string]any{"refresh_token": "new-tok"}
	now := time.Now()

	old := &model.Storage{Modified: now.Add(-time.Minute)}
	oldAddition := map[string]any{"refresh_token": "old-tok"}
	if rowCarriesWrite(old, oldAddition, oldAddition, written, now.Add(-time.Second), now) {
		t.Fatal("row without our write detected as carrying it")
	}

	// The driver rotated the value away, but the modification time is ours.
	rotated := map[string]any{"refresh_token": "rotated"}
	fresh := &model.Storage{Modified: now.Add(-10 * time.Millisecond)}
	if !rowCarriesWrite(fresh, rotated, oldAddition, written, now.Add(-50*time.Millisecond), now) {
		t.Fatal("row with our modification time not detected")
	}

	// The value is still there even though the timestamp is not ours.
	stale := &model.Storage{Modified: now.Add(-time.Minute)}
	if !rowCarriesWrite(stale, map[string]any{"refresh_token": "new-tok"}, oldAddition, written, now.Add(-time.Second), now) {
		t.Fatal("row holding the written value not detected")
	}

	// A written value equal to the original one (the same username) is no
	// evidence of the write.
	sameUser := map[string]any{"username": "admin", "password": "new-pw"}
	origUser := map[string]any{"username": "admin", "password": "old-pw"}
	if rowCarriesWrite(old, origUser, origUser, sameUser, now.Add(-time.Second), now) {
		t.Fatal("unchanged username taken as evidence of the write")
	}
	if !rowCarriesWrite(old, map[string]any{"username": "admin", "password": "new-pw"}, origUser, sameUser, now.Add(-time.Second), now) {
		t.Fatal("written password not detected next to an unchanged username")
	}
}

func TestAdditionRestoredWithUnchangedCredentialField(t *testing.T) {
	credKeys := map[string]bool{"username": true, "password": true, "token": true}
	orig := map[string]any{"username": "admin", "password": "old-pw", "token": "old-tok", "url": "http://x"}
	written := map[string]any{"username": "admin", "password": "new-pw", "token": ""}

	if !additionRestored(orig, orig, credKeys, written) {
		t.Fatal("original values back in place, including the unchanged username, must count as restored")
	}
	rotated := map[string]any{"username": "admin", "password": "old-pw", "token": "rotated-tok", "url": "http://x"}
	if !additionRestored(rotated, orig, credKeys, written) {
		t.Fatal("a token the driver rotated to must count as restored")
	}
	leftover := map[string]any{"username": "admin", "password": "new-pw", "token": "old-tok", "url": "http://x"}
	if additionRestored(leftover, orig, credKeys, written) {
		t.Fatal("the new password still in place must not count as restored")
	}
	moved := map[string]any{"username": "admin", "password": "old-pw", "token": "old-tok", "url": "http://y"}
	if additionRestored(moved, orig, credKeys, written) {
		t.Fatal("a changed non-credential setting must not count as restored")
	}
}

// The usual re-authorization keeps the account and replaces the password:
// with the same username written, a failed attempt must still be detected,
// restored and reported as restored.
func TestReauthSameUsernameRestoreReported(t *testing.T) {
	adapter := newFakeAdapter()
	ctx := context.Background()
	created := Create(ctx, adapter, fakeApp(), fakeAuth("good", map[string]any{"client_id": "same-id"}), CreateInput{MountPath: "/t-reauth-sameuser"})
	if !created.OK {
		t.Fatalf("setup: %+v", created)
	}
	before := *mustRow(t, created.StorageID)
	res := Reauth(ctx, adapter, fakeApp(), fakeAuth("bad-same-user-7777", map[string]any{"client_id": "same-id"}), TargetOf(&before))
	if res.OK || !res.CredentialsWritten {
		t.Fatalf("result: %+v", res)
	}
	if res.Restore == nil || !res.Restore.Attempted || !res.Restore.Restored || res.Restore.Status != op.WORK {
		t.Fatalf("restore report: %+v", res.Restore)
	}
	addition := mustDecode(t, mustRow(t, created.StorageID).Addition)
	if addition["refresh_token"] != "good" || addition["client_id"] != "same-id" {
		t.Fatalf("row after restore: %v", addition)
	}
}

func TestNoSecretsInLogsAndResults(t *testing.T) {
	adapter := newFakeAdapter()
	ctx := context.Background()

	var buf bytes.Buffer
	oldOut := log.StandardLogger().Out
	oldLevel := log.GetLevel()
	log.SetOutput(&buf)
	log.SetLevel(log.InfoLevel)
	defer func() {
		log.SetOutput(oldOut)
		log.SetLevel(oldLevel)
	}()

	createToken := "bad-log-create-0001"
	resCreate := Create(ctx, adapter, fakeApp(), fakeAuth(createToken, nil), CreateInput{MountPath: "/t-log-create"})
	assertNoSecret(t, "log-flow create", createToken, resultTexts(resCreate)...)

	created := Create(ctx, adapter, fakeApp(), fakeAuth("good", nil), CreateInput{MountPath: "/t-log-reauth"})
	if !created.OK {
		t.Fatalf("setup: %+v", created)
	}
	target := TargetOf(mustRow(t, created.StorageID))

	reauthToken := "bad-log-reauth-0002"
	resReauth := Reauth(ctx, adapter, fakeApp(), fakeAuth(reauthToken, nil), target)
	assertNoSecret(t, "log-flow reauth", reauthToken, resultTexts(resReauth)...)

	// A reauth on a storage whose original credential is broken exercises the
	// status text of the restored row.
	badOrig := "bad-log-orig-0003"
	id, err := op.CreateStorage(ctx, model.Storage{
		Driver:    "ConnectFake",
		MountPath: "/t-log-origbad",
		Addition:  fmt.Sprintf(`{"refresh_token":%q,"root_folder_path":"/"}`, badOrig),
	})
	if err == nil {
		t.Fatal("setup create should have failed its init")
	}
	resBad := Reauth(ctx, adapter, fakeApp(), fakeAuth("bad-log-new-0004", nil), TargetOf(mustRow(t, id)))
	assertNoSecret(t, "log-flow origbad", badOrig, resultTexts(resBad)...)
	assertNoSecret(t, "log-flow origbad new", "bad-log-new-0004", resultTexts(resBad)...)

	logs := buf.String()
	for _, secret := range []string{createToken, reauthToken, badOrig, "bad-log-new-0004"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("log output leaks %q:\n%s", secret, logs)
		}
	}
}

func TestCreateInitFailureDropErrorStillCleansUp(t *testing.T) {
	adapter := newFakeAdapter()
	res := Create(context.Background(), adapter, fakeApp(), fakeAuth("bad-drop-token-4321", nil), CreateInput{MountPath: "/t-drop-fail"})
	if res.OK || res.StorageID == 0 {
		t.Fatalf("result: %+v", res)
	}
	// Drop reports an error but the row is gone; only the re-read decides.
	if res.Cleanup == nil || !res.Cleanup.Attempted || !res.Cleanup.Deleted || res.Cleanup.Error != "" {
		t.Fatalf("cleanup report: %+v", res.Cleanup)
	}
	assertGone(t, res.StorageID)
}

func TestCreateCleanupSkipsRowChangedMeanwhile(t *testing.T) {
	adapter := newFakeAdapter()
	// The failed row is edited (moved to another mount path) before the
	// cleanup looks at it: it is no longer the storage this attempt created.
	afterCreateHook = func(id uint) {
		row, err := db.GetStorageById(id)
		if err != nil {
			t.Errorf("hook read: %v", err)
			return
		}
		row.MountPath = "/t-cleanup-moved"
		if err := db.UpdateStorage(row); err != nil {
			t.Errorf("hook update: %v", err)
		}
	}
	defer func() { afterCreateHook = nil }()

	res := Create(context.Background(), adapter, fakeApp(), fakeAuth("bad-moved-1111", nil), CreateInput{MountPath: "/t-cleanup-orig"})
	if res.OK || res.StorageID == 0 {
		t.Fatalf("result: %+v", res)
	}
	if res.Cleanup == nil || res.Cleanup.Attempted || res.Cleanup.Deleted || res.Cleanup.Error == "" {
		t.Fatalf("cleanup must leave a changed row alone: %+v", res.Cleanup)
	}
	if !strings.Contains(res.Message, "请在 OpenList 中手动删除") {
		t.Fatalf("message %q", res.Message)
	}
	if row := mustRow(t, res.StorageID); row.MountPath != "/t-cleanup-moved" {
		t.Fatalf("row was modified by the cleanup: %+v", row)
	}
	if err := db.DeleteStorageById(res.StorageID); err != nil {
		t.Fatalf("tidy up: %v", err)
	}
}

func TestReauthIgnoresCreateFields(t *testing.T) {
	adapter := newFakeAdapter()
	ctx := context.Background()
	createAuth := fakeAuth("good", nil)
	createAuth.CreateFields = map[string]any{"keep": "orig"}
	created := Create(ctx, adapter, fakeApp(), createAuth, CreateInput{MountPath: "/t-reauth-createfields"})
	if !created.OK {
		t.Fatalf("setup: %+v", created)
	}
	before := *mustRow(t, created.StorageID)

	auth := fakeAuth("good-2", nil)
	auth.CreateFields = map[string]any{"keep": "changed"}
	res := Reauth(ctx, adapter, fakeApp(), auth, TargetOf(&before))
	if !res.OK {
		t.Fatalf("result: %+v", res)
	}
	addition := mustDecode(t, mustRow(t, created.StorageID).Addition)
	if addition["refresh_token"] != "good-2" {
		t.Fatalf("credential not replaced: %v", addition["refresh_token"])
	}
	if addition["keep"] != "orig" {
		t.Fatalf("re-authorization wrote a create-only field: keep=%v", addition["keep"])
	}
}

func TestReauthRestoreVerificationFailureIsReported(t *testing.T) {
	adapter := newFakeAdapter()
	ctx := context.Background()
	created := Create(ctx, adapter, fakeApp(), fakeAuth("good", nil), CreateInput{MountPath: "/t-restore-verify", Remark: "orig remark"})
	if !created.OK {
		t.Fatalf("setup: %+v", created)
	}
	before := *mustRow(t, created.StorageID)
	// An edit lands right after the restore: the re-read no longer shows the
	// original settings, so the restore must not be reported as done.
	afterRestoreHook = func(id uint) {
		row, err := db.GetStorageById(id)
		if err != nil {
			t.Errorf("hook read: %v", err)
			return
		}
		row.Remark = "edited during restore"
		if err := db.UpdateStorage(row); err != nil {
			t.Errorf("hook update: %v", err)
		}
	}
	defer func() { afterRestoreHook = nil }()

	res := Reauth(ctx, adapter, fakeApp(), fakeAuth("bad-verify-2222", nil), TargetOf(&before))
	if res.OK || !res.CredentialsWritten {
		t.Fatalf("result: %+v", res)
	}
	if res.Restore == nil || !res.Restore.Attempted || res.Restore.Restored || res.Restore.Error == "" {
		t.Fatalf("restore report: %+v", res.Restore)
	}
	if !strings.Contains(res.Message, "未能恢复原配置") {
		t.Fatalf("message %q", res.Message)
	}
}

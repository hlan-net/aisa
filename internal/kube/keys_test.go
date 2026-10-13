package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/hlan-net/aisa/internal/consumers"
	"github.com/hlan-net/aisa/internal/vault"
)

// memKV is Vault's KV v2 in memory: data goes through JSON, as it does to and from Vault.
type memKV struct {
	data     map[string][]byte
	versions map[string]int
	writes   int
	// fail, when set, is returned by the write with this number (1 is the first).
	failWrite int
	fail      error
}

func newMemKV() *memKV { return &memKV{data: map[string][]byte{}, versions: map[string]int{}} }

// List lists the secrets under a path, deleted ones included, as Vault's metadata does.
func (m *memKV) List(_ context.Context, mount, path string) ([]string, error) {
	var names []string
	for key := range m.data {
		if name, ok := strings.CutPrefix(key, mount+"/"+path+"/"); ok {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, vault.ErrNotFound
	}
	sort.Strings(names)
	return names, nil
}

func (m *memKV) ReadVersion(_ context.Context, mount, path string) (map[string]any, int, error) {
	b, ok := m.data[mount+"/"+path]
	if !ok || b == nil {
		// Never written, or its current version deleted: Vault answers with that version.
		return nil, m.versions[mount+"/"+path], vault.ErrNotFound
	}
	var data map[string]any
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, 0, err
	}
	return data, m.versions[mount+"/"+path], nil
}

func (m *memKV) Write(_ context.Context, mount, path string, data map[string]any, cas int) error {
	m.writes++
	if m.writes == m.failWrite {
		return m.fail
	}
	key := mount + "/" + path
	if cas != m.versions[key] {
		return vault.ErrConflict
	}
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	m.data[key] = b
	m.versions[key]++
	return nil
}

// del deletes the current version, as `vault kv delete` does: the version stays.
func (m *memKV) del(name string) { m.data["secret/aisa/consumers/"+name] = nil }

func (m *memKV) put(name string, data map[string]any) {
	b, _ := json.Marshal(data)
	m.data["secret/aisa/consumers/"+name] = b
	m.versions["secret/aisa/consumers/"+name]++
}

func (m *memKV) get(t *testing.T, name string) map[string]any {
	t.Helper()
	data, _, err := m.ReadVersion(context.Background(), "secret", "aisa/consumers/"+name)
	if err != nil {
		t.Fatalf("read %s from Vault: %v", name, err)
	}
	return data
}

// hashes returns the consumer's valid hashes, sorted.
func (m *memKV) hashes(t *testing.T, name string) []string {
	t.Helper()
	s, _ := m.get(t, name)[fieldHashes].(string)
	h := splitHashes(s)
	sort.Strings(h)
	return h
}

type staticDecls struct {
	valid   map[string]consumers.Declaration
	invalid map[string]bool
}

func (d *staticDecls) Snapshot() (map[string]consumers.Declaration, map[string]bool, bool) {
	return d.valid, d.invalid, d.valid != nil
}

type keysTest struct {
	t      *testing.T
	kv     *memKV
	k8s    *fake.Clientset
	decls  *staticDecls
	now    time.Time
	issued []string
	states map[string]int
	err    error
	keys   *Keys
}

const baseURL = "http://aisa-apisix.aisa.svc.cluster.local/v1"

func newKeysTest(t *testing.T, decls map[string]consumers.Declaration) *keysTest {
	kt := &keysTest{
		t:     t,
		kv:    newMemKV(),
		k8s:   fake.NewClientset(),
		decls: &staticDecls{valid: decls, invalid: map[string]bool{}},
		now:   time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC),
	}
	seq := byte(0)
	kt.keys = NewKeys(kt.kv, kt.k8s, kt.decls, slog.New(slog.DiscardHandler), KeysOptions{
		Mount: "secret", Prefix: "aisa", BaseURL: baseURL, Install: "aisa",
		Rotation: 30 * 24 * time.Hour, Grace: 7 * 24 * time.Hour,
		Now: func() time.Time { return kt.now },
		// Each key differs from the last.
		Rand:    readerFunc(func(b []byte) (int, error) { seq++; return copy(b, bytes.Repeat([]byte{seq}, len(b))), nil }),
		OnIssue: func(reason string) { kt.issued = append(kt.issued, reason) },
		OnPass:  func(states map[string]int) { kt.states = states },
	})
	return kt
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(b []byte) (int, error) { return f(b) }

func (kt *keysTest) pass() {
	kt.t.Helper()
	kt.err = kt.keys.Reconcile(context.Background())
}

func (kt *keysTest) secret(ns, name string) *corev1.Secret {
	kt.t.Helper()
	s, err := kt.k8s.CoreV1().Secrets(ns).Get(context.Background(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		kt.t.Fatal(err)
	}
	return s
}

// key returns the key in the consumer's Secret, failing when there is none.
func (kt *keysTest) key(ns, name string) string {
	kt.t.Helper()
	s := kt.secret(ns, name)
	if s == nil {
		kt.t.Fatalf("no Secret %s/%s", ns, name)
	}
	return string(s.Data[SecretKey])
}

func news() map[string]consumers.Declaration {
	return map[string]consumers.Declaration{"batch-jobs": {QuotaProfile: "batch", Namespace: "news", Secret: "aisa"}}
}

func TestIssueForANewConsumer(t *testing.T) {
	kt := newKeysTest(t, news())
	kt.pass()

	s := kt.secret("news", "aisa")
	if s == nil {
		t.Fatal("no Secret was written")
	}
	key := string(s.Data[SecretKey])
	if !strings.HasPrefix(key, "aisa-") || string(s.Data[SecretBaseURL]) != baseURL {
		t.Errorf("Secret data = %q", s.Data)
	}
	if s.Labels[LabelConsumer] != DerivedName("batch-jobs", "consumer") || s.Annotations[AnnotationConsumer] != "batch-jobs" ||
		s.Labels[LabelManagedBy] != ManagedBy || s.Labels[LabelInstall] != "aisa" {
		t.Errorf("Secret labels %v, annotations %v", s.Labels, s.Annotations)
	}
	if got := kt.kv.hashes(t, "batch-jobs"); !slices.Equal(got, []string{hashKey(key)}) {
		t.Errorf("Vault hashes = %v, want the key's", got)
	}
	data := kt.kv.get(t, "batch-jobs")
	if _, ok := data[fieldUnwritten]; ok {
		t.Errorf("the mark was left: %v", data)
	}
	if data[fieldIssuedAt] != "2026-10-13T12:00:00Z" {
		t.Errorf("issued_at = %v", data[fieldIssuedAt])
	}
	if !slices.Equal(kt.issued, []string{IssueNew}) || kt.states[StateOK] != 1 {
		t.Errorf("issued %v, states %v", kt.issued, kt.states)
	}

	// A pass with nothing to do writes nothing.
	writes := kt.kv.writes
	kt.pass()
	if kt.kv.writes != writes || kt.key("news", "aisa") != key || len(kt.issued) != 1 {
		t.Errorf("a second pass changed something: %d writes, issued %v", kt.kv.writes-writes, kt.issued)
	}
}

func TestAMissingSecretGetsANewKeyAndTheOldOneAGracePeriod(t *testing.T) {
	kt := newKeysTest(t, news())
	kt.pass()
	old := kt.key("news", "aisa")
	if err := kt.k8s.CoreV1().Secrets("news").Delete(context.Background(), "aisa", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}

	kt.now = kt.now.Add(time.Hour)
	kt.pass()
	key := kt.key("news", "aisa")
	if key == old {
		t.Fatal("the Secret came back with the old key")
	}
	want := []string{hashKey(old), hashKey(key)}
	sort.Strings(want)
	if got := kt.kv.hashes(t, "batch-jobs"); !slices.Equal(got, want) {
		t.Errorf("hashes = %v, want the old and the new", got)
	}
	if kt.issued[len(kt.issued)-1] != IssueMissing {
		t.Errorf("issued %v, want missing last", kt.issued)
	}

	// After the grace period the old key goes.
	kt.now = kt.now.Add(7*24*time.Hour + time.Second)
	kt.pass()
	if got := kt.kv.hashes(t, "batch-jobs"); !slices.Equal(got, []string{hashKey(key)}) {
		t.Errorf("hashes after the grace period = %v, want only the new key's", got)
	}
	if _, ok := kt.kv.get(t, "batch-jobs")[fieldSuperseded]; ok {
		t.Error("the superseded mark was left after the grace period")
	}
}

func TestAKeyEditedByHandIsNeverAccepted(t *testing.T) {
	kt := newKeysTest(t, news())
	kt.pass()
	s := kt.secret("news", "aisa")
	s.Data[SecretKey] = []byte("chosen-by-hand")
	if _, err := kt.k8s.CoreV1().Secrets("news").Update(context.Background(), s, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	kt.pass()
	key := kt.key("news", "aisa")
	if key == "chosen-by-hand" {
		t.Fatal("the key edited by hand stayed")
	}
	if slices.Contains(kt.kv.hashes(t, "batch-jobs"), hashKey("chosen-by-hand")) {
		t.Fatal("the key edited by hand became valid")
	}
	if kt.issued[len(kt.issued)-1] != IssueUnknownKey {
		t.Errorf("issued %v, want unknown_key last", kt.issued)
	}
}

func TestKeysRotate(t *testing.T) {
	kt := newKeysTest(t, news())
	kt.pass()
	old := kt.key("news", "aisa")

	kt.now = kt.now.Add(30*24*time.Hour - time.Second)
	kt.pass()
	if kt.key("news", "aisa") != old {
		t.Fatal("rotated before the interval")
	}
	kt.now = kt.now.Add(time.Second)
	kt.pass()
	key := kt.key("news", "aisa")
	if key == old || kt.issued[len(kt.issued)-1] != IssueRotation {
		t.Fatalf("not rotated at the interval: issued %v", kt.issued)
	}
	if got := kt.kv.hashes(t, "batch-jobs"); len(got) != 2 {
		t.Errorf("hashes = %v, want the old one through its grace period and the new one", got)
	}
}

func TestAKeyFromBeforeIssuedAtStartsItsAgeNow(t *testing.T) {
	kt := newKeysTest(t, news())
	kt.kv.put("batch-jobs", map[string]any{fieldHashes: hashKey("aisa-by-up-sh"), "quota_profile": "batch"})
	if _, err := kt.k8s.CoreV1().Secrets("news").Create(context.Background(), kt.ownedSecret("batch-jobs", "news", "aisa", "aisa-by-up-sh"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	kt.pass()
	data := kt.kv.get(t, "batch-jobs")
	if kt.key("news", "aisa") != "aisa-by-up-sh" || data[fieldIssuedAt] != "2026-10-13T12:00:00Z" || data["quota_profile"] != "batch" {
		t.Errorf("Vault = %v, issued %v", data, kt.issued)
	}
}

func (kt *keysTest) ownedSecret(consumer, ns, name, key string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Labels: map[string]string{
				LabelManagedBy: ManagedBy, LabelInstall: "aisa", LabelConsumer: DerivedName(consumer, "consumer"),
			},
			Annotations: map[string]string{AnnotationConsumer: consumer},
		},
		Data: map[string][]byte{SecretKey: []byte(key), SecretBaseURL: []byte(baseURL)},
	}
}

func TestALeftoverMarkIsCleared(t *testing.T) {
	// A crash after the Secret was written, before the mark was cleared.
	kt := newKeysTest(t, news())
	kt.kv.put("batch-jobs", map[string]any{
		fieldHashes:    hashKey("aisa-k"),
		fieldUnwritten: map[string]any{hashKey("aisa-k"): "2026-10-13T11:00:00Z"},
		fieldIssuedAt:  "2026-10-13T11:00:00Z",
	})
	if _, err := kt.k8s.CoreV1().Secrets("news").Create(context.Background(), kt.ownedSecret("batch-jobs", "news", "aisa", "aisa-k"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	kt.pass()
	if _, ok := kt.kv.get(t, "batch-jobs")[fieldUnwritten]; ok || kt.key("news", "aisa") != "aisa-k" || len(kt.issued) != 0 {
		t.Errorf("Vault = %v, issued %v", kt.kv.get(t, "batch-jobs"), kt.issued)
	}
}

func TestAnUnwrittenKeyThatNobodyHoldsIsRemoved(t *testing.T) {
	// A crash after the write to Vault, before the Secret was written.
	kt := newKeysTest(t, news())
	kt.pass()
	key := kt.key("news", "aisa")
	kt.kv.put("batch-jobs", map[string]any{
		fieldHashes:    hashKey(key) + "," + hashKey("aisa-lost"),
		fieldUnwritten: map[string]any{hashKey("aisa-lost"): "2026-10-13T12:00:00Z"},
		fieldIssuedAt:  "2026-10-13T12:00:00Z",
	})

	kt.now = kt.now.Add(30 * time.Second)
	kt.pass()
	if !slices.Contains(kt.kv.hashes(t, "batch-jobs"), hashKey("aisa-lost")) {
		t.Fatal("removed within the minute that the Secret's write may take")
	}
	kt.now = kt.now.Add(time.Minute)
	kt.pass()
	if got := kt.kv.hashes(t, "batch-jobs"); !slices.Equal(got, []string{hashKey(key)}) {
		t.Errorf("hashes = %v, want only the key in the Secret", got)
	}
}

func TestTheBaseURLIsRestored(t *testing.T) {
	kt := newKeysTest(t, news())
	kt.pass()
	key := kt.key("news", "aisa")
	s := kt.secret("news", "aisa")
	s.Data[SecretBaseURL] = []byte("http://elsewhere/v1")
	if _, err := kt.k8s.CoreV1().Secrets("news").Update(context.Background(), s, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	kt.pass()
	s = kt.secret("news", "aisa")
	if string(s.Data[SecretBaseURL]) != baseURL || string(s.Data[SecretKey]) != key {
		t.Errorf("Secret data = %q", s.Data)
	}
}

func TestASecretThatIsNotTheConsumersIsLeftAlone(t *testing.T) {
	for name, s := range map[string]*corev1.Secret{
		"no label":         {ObjectMeta: metav1.ObjectMeta{Name: "aisa", Namespace: "news"}, Data: map[string][]byte{"password": []byte("x")}},
		"another consumer": (&keysTest{}).ownedSecret("chat-ui", "news", "aisa", "aisa-x"),
		"another install": func() *corev1.Secret {
			s := (&keysTest{}).ownedSecret("batch-jobs", "news", "aisa", "aisa-x")
			s.Labels[LabelInstall] = "staging"
			return s
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			kt := newKeysTest(t, news())
			// chat-ui's declaration cannot be read, so its Secret is not a removed consumer's.
			kt.decls.invalid["chat-ui"] = true
			if _, err := kt.k8s.CoreV1().Secrets("news").Create(context.Background(), s, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			kt.pass()
			got := kt.secret("news", "aisa")
			if got == nil || !equalData(got.Data, s.Data) {
				t.Errorf("the Secret was changed: %q", got)
			}
			if kt.states[StateNotOwned] != 1 || len(kt.issued) != 0 || kt.kv.writes != 0 {
				t.Errorf("states %v, issued %v, %d writes to Vault", kt.states, kt.issued, kt.kv.writes)
			}
		})
	}
}

func equalData(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !bytes.Equal(v, b[k]) {
			return false
		}
	}
	return true
}

func TestConsumersThatNameOneSecretAreNotReconciled(t *testing.T) {
	kt := newKeysTest(t, map[string]consumers.Declaration{
		"a": {Namespace: "news", Secret: "aisa"},
		"b": {Namespace: "news", Secret: "aisa"},
		"c": {Namespace: "news", Secret: "other"},
	})
	kt.pass()
	if kt.secret("news", "aisa") != nil {
		t.Error("a Secret named by two consumers was written")
	}
	if kt.secret("news", "other") == nil {
		t.Error("the third consumer did not get its Secret")
	}
	if kt.states[StateDuplicate] != 2 || kt.states[StateOK] != 1 {
		t.Errorf("states = %v", kt.states)
	}
}

func TestConsumersMovedOntoOneSecretKeepTheirOldOnes(t *testing.T) {
	kt := newKeysTest(t, map[string]consumers.Declaration{
		"a": {Namespace: "news", Secret: "a"},
		"b": {Namespace: "news", Secret: "b"},
	})
	kt.pass()
	keyA, keyB := kt.key("news", "a"), kt.key("news", "b")
	// Both are moved onto one Secret: a conflict, which leaves their Secrets and hashes alone.
	kt.decls.valid = map[string]consumers.Declaration{
		"a": {Namespace: "news", Secret: "shared"},
		"b": {Namespace: "news", Secret: "shared"},
	}
	kt.pass()
	if kt.err != nil || kt.states[StateDuplicate] != 2 {
		t.Fatalf("states %v, err %v", kt.states, kt.err)
	}
	if kt.key("news", "a") != keyA || kt.key("news", "b") != keyB || kt.secret("news", "shared") != nil {
		t.Error("a conflict changed the consumers' Secrets")
	}
	if !slices.Equal(kt.kv.hashes(t, "a"), []string{hashKey(keyA)}) || !slices.Equal(kt.kv.hashes(t, "b"), []string{hashKey(keyB)}) {
		t.Error("a conflict changed the consumers' hashes")
	}
}

func TestARemovedConsumerLosesItsSecretAndItsHashes(t *testing.T) {
	for name, decls := range map[string]map[string]consumers.Declaration{
		"undeclared":          {},
		"declared, no Secret": {"batch-jobs": {QuotaProfile: "batch"}},
	} {
		t.Run(name, func(t *testing.T) {
			kt := newKeysTest(t, news())
			kt.pass()
			kt.kv.put("batch-jobs", mergeData(kt.kv.get(t, "batch-jobs"), map[string]any{"note": "kept"}))
			kt.decls.valid = decls
			kt.pass()
			if kt.secret("news", "aisa") != nil {
				t.Error("the Secret stayed")
			}
			data := kt.kv.get(t, "batch-jobs")
			if data[fieldHashes] != "" || data[fieldIssuedAt] != nil || data["note"] != "kept" {
				t.Errorf("Vault = %v, want no hashes and the other fields kept", data)
			}
		})
	}
}

func mergeData(a, b map[string]any) map[string]any {
	for k, v := range b {
		a[k] = v
	}
	return a
}

func TestAMovedConsumerGetsANewSecretAndKey(t *testing.T) {
	kt := newKeysTest(t, news())
	kt.pass()
	old := kt.key("news", "aisa")
	kt.decls.valid = map[string]consumers.Declaration{"batch-jobs": {Namespace: "reports", Secret: "llm"}}
	kt.pass()
	if kt.secret("news", "aisa") != nil {
		t.Error("the old Secret stayed")
	}
	key := kt.key("reports", "llm")
	if key == old || len(kt.kv.hashes(t, "batch-jobs")) != 2 {
		t.Errorf("hashes %v: want the new key and the old one through its grace period", kt.kv.hashes(t, "batch-jobs"))
	}
}

func TestAMovedConsumerKeepsItsOldSecretUntilTheNewOneIsThere(t *testing.T) {
	kt := newKeysTest(t, news())
	kt.pass()
	old := kt.key("news", "aisa")
	failing := true
	kt.k8s.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failing {
			return true, nil, errors.New(`namespaces "reports" not found`)
		}
		return false, nil, nil
	})
	kt.decls.valid = map[string]consumers.Declaration{"batch-jobs": {Namespace: "reports", Secret: "llm"}}
	kt.pass()
	if kt.states[StateError] != 1 || kt.key("news", "aisa") != old {
		t.Fatalf("states %v: the old Secret went before the new one was written", kt.states)
	}
	// However long the move fails, the old key stays valid: its grace period has not begun.
	kt.now = kt.now.Add(8 * 24 * time.Hour)
	kt.pass()
	if !slices.Contains(kt.kv.hashes(t, "batch-jobs"), hashKey(old)) {
		t.Fatal("the old key was revoked while the move failed")
	}
	failing = false
	kt.now = kt.now.Add(time.Hour)
	kt.pass()
	if kt.secret("news", "aisa") != nil || kt.key("reports", "llm") == "" {
		t.Error("want the new Secret written and then the old one deleted")
	}
	if kt.kv.get(t, "batch-jobs")[fieldSuperseded] == nil {
		t.Error("the old key's grace period did not begin once the new Secret was written")
	}
}

func TestAKeyWrittenBeforeACrashSupersedesTheOldOnes(t *testing.T) {
	kt := newKeysTest(t, news())
	kt.pass()
	old := kt.key("news", "aisa")
	// A rotation whose last write, clearing the mark, fails: the next pass finishes it.
	kt.now = kt.now.Add(30 * 24 * time.Hour)
	kt.kv.failWrite, kt.kv.fail = kt.kv.writes+2, errors.New("vault is down")
	kt.pass()
	kt.kv.fail = nil
	kt.pass()
	data := kt.kv.get(t, "batch-jobs")
	if kt.key("news", "aisa") == old || data[fieldUnwritten] != nil || data[fieldSuperseded] == nil {
		t.Errorf("Vault = %v: want the mark cleared and the old key superseded", data)
	}
	kt.now = kt.now.Add(8 * 24 * time.Hour)
	kt.pass()
	if slices.Contains(kt.kv.hashes(t, "batch-jobs"), hashKey(old)) {
		t.Error("the old key outlived its grace period")
	}
}

func TestAConsumerWhoseDeclarationCannotBeReadKeepsItsSecret(t *testing.T) {
	kt := newKeysTest(t, news())
	kt.pass()
	kt.decls.valid = map[string]consumers.Declaration{}
	kt.decls.invalid = map[string]bool{"batch-jobs": true}
	kt.pass()
	if kt.secret("news", "aisa") == nil || len(kt.kv.hashes(t, "batch-jobs")) != 1 {
		t.Error("a consumer with a broken declaration lost its Secret or its key")
	}
}

func TestNothingBeforeTheDeclarationsAreRead(t *testing.T) {
	kt := newKeysTest(t, nil)
	if _, err := kt.k8s.CoreV1().Secrets("news").Create(context.Background(), kt.ownedSecret("batch-jobs", "news", "aisa", "aisa-k"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	kt.pass()
	if kt.secret("news", "aisa") == nil || kt.states != nil {
		t.Error("a pass before the declarations were read changed something")
	}
}

func TestAFailedWriteIsFinishedByTheNextPass(t *testing.T) {
	for _, step := range []int{1, 2} {
		t.Run(fmt.Sprintf("write %d fails", step), func(t *testing.T) {
			kt := newKeysTest(t, news())
			kt.kv.failWrite, kt.kv.fail = step, errors.New("vault is down")
			kt.pass()
			if kt.states[StateError] != 1 {
				t.Fatalf("states = %v, want an error", kt.states)
			}
			kt.now = kt.now.Add(2 * time.Minute)
			kt.pass()
			kt.checkIssuedOnce()
		})
	}
}

// checkIssuedOnce checks that batch-jobs has one key, in its Secret, issued and counted once.
func (kt *keysTest) checkIssuedOnce() {
	t := kt.t
	t.Helper()
	key := kt.key("news", "aisa")
	if got := kt.kv.hashes(t, "batch-jobs"); !slices.Equal(got, []string{hashKey(key)}) {
		t.Errorf("hashes = %v, want only the key in the Secret", got)
	}
	data := kt.kv.get(t, "batch-jobs")
	if _, ok := data[fieldUnwritten]; ok {
		t.Error("a mark was left")
	}
	if data[fieldIssuedAt] == nil {
		t.Error("no issued_at")
	}
	// Counted once, also when the key reached the Secret and only its mark was left.
	if !slices.Equal(kt.issued, []string{IssueNew}) {
		t.Errorf("issued %v", kt.issued)
	}
}

func TestARotationWhoseSecretWriteFailedIsRetried(t *testing.T) {
	kt := newKeysTest(t, news())
	kt.pass()
	old := kt.key("news", "aisa")
	failing := true
	kt.k8s.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failing {
			return true, nil, errors.New("the API server is down")
		}
		return false, nil, nil
	})
	kt.now = kt.now.Add(30 * 24 * time.Hour)
	kt.pass()
	if kt.states[StateError] != 1 || kt.key("news", "aisa") != old {
		t.Fatalf("states = %v", kt.states)
	}
	// The Secret still holds the old key: it stays due, and is replaced after the backoff.
	failing = false
	kt.now = kt.now.Add(2 * time.Minute)
	kt.pass()
	if kt.key("news", "aisa") == old || !slices.Equal(kt.issued, []string{IssueNew, IssueRotation}) {
		t.Errorf("not rotated after the failed write: issued %v", kt.issued)
	}
}

func TestAConcurrentWriteToVaultIsNotOverwritten(t *testing.T) {
	kt := newKeysTest(t, news())
	kt.kv.failWrite, kt.kv.fail = 1, vault.ErrConflict
	kt.pass()
	if kt.states[StateError] != 1 || kt.secret("news", "aisa") != nil {
		t.Errorf("states %v: a conflict must stop the issue before the Secret is written", kt.states)
	}
}

func TestANameWithASlashIsRefused(t *testing.T) {
	kt := newKeysTest(t, map[string]consumers.Declaration{"team/app": {Namespace: "news", Secret: "aisa"}})
	kt.pass()
	if kt.secret("news", "aisa") != nil || kt.states[StateError] != 1 {
		t.Errorf("states %v", kt.states)
	}
}

func TestARestoreAndARotationInOnePass(t *testing.T) {
	kt := newKeysTest(t, news())
	kt.pass()
	old := kt.key("news", "aisa")
	s := kt.secret("news", "aisa")
	s.Data[SecretBaseURL] = []byte("http://elsewhere/v1")
	if _, err := kt.k8s.CoreV1().Secrets("news").Update(context.Background(), s, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	kt.now = kt.now.Add(31 * 24 * time.Hour)
	kt.pass()
	s = kt.secret("news", "aisa")
	if string(s.Data[SecretKey]) == old || string(s.Data[SecretBaseURL]) != baseURL || kt.states[StateOK] != 1 {
		t.Errorf("Secret %q, states %v: want the URL restored and the key rotated in one pass", s.Data, kt.states)
	}
}

func TestASecretThatCannotBeWrittenIsRetriedLater(t *testing.T) {
	kt := newKeysTest(t, news())
	failing := true
	kt.k8s.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failing {
			return true, nil, errors.New(`namespaces "news" not found`)
		}
		return false, nil, nil
	})
	kt.pass()
	if kt.states[StateError] != 1 {
		t.Fatalf("states = %v", kt.states)
	}
	writes := kt.kv.writes
	kt.now = kt.now.Add(30 * time.Second)
	kt.pass()
	if kt.kv.writes != writes {
		t.Errorf("%d writes to Vault within the backoff", kt.kv.writes-writes)
	}
	// After the minute the next attempt fails too, and the wait doubles.
	kt.now = kt.now.Add(time.Minute)
	kt.pass()
	if kt.kv.writes == writes {
		t.Fatal("no attempt after the backoff")
	}
	writes = kt.kv.writes
	kt.now = kt.now.Add(90 * time.Second)
	kt.pass()
	if kt.kv.writes != writes {
		t.Error("the wait did not double")
	}
	failing = false
	kt.now = kt.now.Add(time.Minute)
	kt.pass()
	key := kt.key("news", "aisa")
	if got := kt.kv.hashes(t, "batch-jobs"); !slices.Equal(got, []string{hashKey(key)}) {
		t.Errorf("hashes = %v, want only the key in the Secret: the unwritten ones are removed", got)
	}
}

func TestAKeyIsIssuedOnADeletedVaultSecret(t *testing.T) {
	kt := newKeysTest(t, news())
	kt.pass()
	if err := kt.k8s.CoreV1().Secrets("news").Delete(context.Background(), "aisa", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	kt.kv.del("batch-jobs")
	kt.pass()
	if kt.err != nil || kt.secret("news", "aisa") == nil {
		t.Fatalf("err %v: want a key issued on the deleted secret's version", kt.err)
	}
}

func TestADroppedSecretTargetRevokesTheKeysFromVault(t *testing.T) {
	// The Secret is gone before the declaration stops naming it: nothing in Kubernetes is left
	// to start the revocation from.
	kt := newKeysTest(t, news())
	kt.pass()
	if err := kt.k8s.CoreV1().Secrets("news").Delete(context.Background(), "aisa", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	kt.decls.valid = map[string]consumers.Declaration{"batch-jobs": {QuotaProfile: "batch"}}
	kt.pass()
	if got := kt.kv.hashes(t, "batch-jobs"); len(got) != 0 {
		t.Errorf("hashes = %v, want none", got)
	}
	// A consumer whose hashes the holder wrote by hand is left alone.
	kt.kv.put("chat-ui", map[string]any{fieldHashes: hashKey("by-hand")})
	kt.decls.valid["chat-ui"] = consumers.Declaration{}
	kt.pass()
	if got := kt.kv.hashes(t, "chat-ui"); len(got) != 1 {
		t.Errorf("hand-written hashes = %v, want them kept", got)
	}
}

func TestTheSecretStaysUntilItsHashesAreRevoked(t *testing.T) {
	kt := newKeysTest(t, news())
	kt.pass()
	kt.decls.valid = map[string]consumers.Declaration{}
	kt.kv.failWrite, kt.kv.fail = kt.kv.writes+1, errors.New("vault is down")
	kt.pass()
	if kt.err == nil || kt.secret("news", "aisa") == nil {
		t.Fatalf("err %v: want an error and the Secret kept while its hashes are valid", kt.err)
	}
	kt.pass()
	if kt.err != nil || kt.secret("news", "aisa") != nil || len(kt.kv.hashes(t, "batch-jobs")) != 0 {
		t.Errorf("err %v: want the hashes revoked and the Secret deleted on the next pass", kt.err)
	}
}

func TestAPassWithAFailedConsumerIsAnError(t *testing.T) {
	kt := newKeysTest(t, map[string]consumers.Declaration{
		"team/app": {Namespace: "news", Secret: "a"},
		"fine":     {Namespace: "news", Secret: "b"},
	})
	kt.pass()
	if kt.err == nil || !strings.Contains(kt.err.Error(), "team/app") || kt.secret("news", "b") == nil {
		t.Errorf("err = %v, want the failed consumer named, and the others reconciled", kt.err)
	}
}

func TestAConsumerRemovedAfterItsSecretLosesItsHashes(t *testing.T) {
	// The Secret goes first, then the declaration: neither is left to name the consumer.
	kt := newKeysTest(t, news())
	kt.pass()
	if err := kt.k8s.CoreV1().Secrets("news").Delete(context.Background(), "aisa", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	kt.decls.valid = map[string]consumers.Declaration{}
	kt.pass()
	if got := kt.kv.hashes(t, "batch-jobs"); len(got) != 0 || kt.err != nil {
		t.Errorf("hashes = %v, err %v: want the issued key revoked from Vault's side", got, kt.err)
	}
	// One whose declaration cannot be read keeps its hashes.
	kt.decls.valid = news()
	kt.pass()
	kt.decls.valid = map[string]consumers.Declaration{}
	kt.decls.invalid = map[string]bool{"batch-jobs": true}
	if err := kt.k8s.CoreV1().Secrets("news").Delete(context.Background(), "aisa", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	kt.pass()
	if got := kt.kv.hashes(t, "batch-jobs"); len(got) == 0 {
		t.Error("a consumer with a broken declaration lost its keys")
	}
}

package kube

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	"github.com/hlan-net/aisa/internal/consumers"
	"github.com/hlan-net/aisa/internal/vault"
)

// The fields of the consumer's Secret (KUBERNETES_OBJECTS.md, The consumer's Secret).
const (
	SecretBaseURL = "OPENAI_BASE_URL"
	SecretKey     = "OPENAI_API_KEY"
)

// The fields of a consumer's secret in Vault that aisa writes besides key_sha256, which the
// decision API reads (docs/concepts/VAULT.md). Each mark is an object of hash → RFC 3339 time.
const (
	fieldHashes     = "key_sha256"
	fieldUnwritten  = "unwritten_sha256"
	fieldSuperseded = "superseded_sha256"
	fieldIssuedAt   = "issued_at"
)

// Why a key was issued, for aisa_consumer_keys_issued_total.
const (
	// IssueNew: a consumer that had neither a key in Vault nor a Secret.
	IssueNew = "new"
	// IssueMissing: the Secret was gone, while Vault had keys for the consumer.
	IssueMissing = "missing"
	// IssueUnknownKey: the Secret's key was not one that aisa issued, or was edited by hand.
	IssueUnknownKey = "unknown_key"
	// IssueRotation: the key was older than the rotation interval.
	IssueRotation = "rotation"
)

// What a pass made of a consumer, for aisa_consumer_secrets{state}.
const (
	StateOK        = "ok"
	StateNotOwned  = "not_owned"
	StateDuplicate = "duplicate_target"
	StateError     = "error"
)

// KV is the part of the Vault client the keys need.
type KV interface {
	ReadVersion(ctx context.Context, mount, path string) (map[string]any, int, error)
	Write(ctx context.Context, mount, path string, data map[string]any, cas int) error
}

// Declarations are the consumers declared in Consul; consumers.Declarations implements it.
type Declarations interface {
	Snapshot() (valid map[string]consumers.Declaration, invalid map[string]bool, loaded bool)
}

// KeysOptions configure Keys.
type KeysOptions struct {
	// Mount and Prefix name the consumers in Vault: <Mount>/<Prefix>/consumers/<name>.
	Mount, Prefix string
	// BaseURL is the proxy's URL that applications call, OPENAI_BASE_URL in the Secret.
	BaseURL string
	// Install names this aisa install on the objects it writes: the namespace it runs in.
	Install string
	// Rotation is the age at which a key is replaced; 0 never replaces one for its age.
	Rotation time.Duration
	// Grace is how long a replaced key stays valid, so that pods that hold it can restart.
	Grace time.Duration
	// Unwritten is the age after which a key whose Secret was never written is removed: a crash
	// between the write to Vault and the write of the Secret left it valid with nobody holding
	// it. 1 minute when zero.
	Unwritten time.Duration
	// Now and Rand are the clock and the source of new keys; tests replace them.
	Now  func() time.Time
	Rand io.Reader
	// OnIssue is called for every key issued, with its reason.
	OnIssue func(reason string)
	// OnPass is called after every pass with the number of consumers in each state.
	OnPass func(states map[string]int)
}

// Keys issues the keys of the consumers that name a namespace and a Secret, and keeps their
// Secrets and the hashes in Vault in step.
type Keys struct {
	kv    KV
	k8s   kubernetes.Interface
	decls Declarations
	opts  KeysOptions
	log   *slog.Logger

	// backoff holds, per consumer, when a key may be issued again after its Secret could not
	// be written (a namespace that does not exist, say), and how long the last wait was, so
	// that a failure does not add a hash to Vault on every pass. Only Reconcile uses it.
	backoff map[string]retry
}

type retry struct {
	at   time.Time
	wait time.Duration
}

// Waits between attempts to issue a key whose Secret could not be written.
const (
	minRetry = time.Minute
	maxRetry = 30 * time.Minute
)

// NewKeys returns Keys; call Reconcile for each pass.
func NewKeys(kv KV, k8s kubernetes.Interface, decls Declarations, log *slog.Logger, opts KeysOptions) *Keys {
	if opts.Unwritten <= 0 {
		opts.Unwritten = time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Rand == nil {
		opts.Rand = rand.Reader
	}
	return &Keys{kv: kv, k8s: k8s, decls: decls, opts: opts, log: log, backoff: map[string]retry{}}
}

// target is where a consumer's Secret goes.
type target struct{ namespace, name string }

// Reconcile makes one pass over the consumers: their Secrets and their hashes in Vault, then
// the Secrets of consumers that no longer name them. A consumer whose pass fails is logged and
// counted, and the next pass tries again. Before the declarations are read it does nothing.
func (k *Keys) Reconcile(ctx context.Context) error {
	valid, invalid, loaded := k.decls.Snapshot()
	if !loaded {
		return nil
	}
	byTarget := map[target][]string{}
	for name, decl := range valid {
		if decl.Namespace != "" {
			t := target{decl.Namespace, decl.Secret}
			byTarget[t] = append(byTarget[t], name)
		}
	}

	states := map[string]int{StateOK: 0, StateNotOwned: 0, StateDuplicate: 0, StateError: 0}
	var errs []error
	for _, t := range sortedTargets(byTarget) {
		names := byTarget[t]
		if len(names) > 1 {
			// One Secret, one consumer: none of them is reconciled until the holder changes one.
			sort.Strings(names)
			k.log.Error("consumers name the same Secret; none of them is reconciled",
				"consumers", names, "namespace", t.namespace, "secret", t.name)
			states[StateDuplicate] += len(names)
			continue
		}
		state, err := k.reconcileConsumer(ctx, names[0], t)
		if err != nil {
			k.log.Error("reconciling the consumer's Secret failed", "consumer", names[0],
				"namespace", t.namespace, "secret", t.name, "error", err)
			if state == StateError {
				errs = append(errs, fmt.Errorf("consumer %q: %w", names[0], err))
			}
		}
		states[state]++
	}

	if err := k.removeStale(ctx, valid, invalid); err != nil {
		k.log.Error("removing the Secrets of removed consumers failed", "error", err)
		errs = append(errs, err)
	}
	if err := k.revokeDropped(ctx, valid); err != nil {
		errs = append(errs, err)
	}
	if k.opts.OnPass != nil {
		k.opts.OnPass(states)
	}
	return errors.Join(errs...)
}

// revokeDropped removes the hashes of the keys aisa issued to consumers that are declared and no
// longer name a Secret, from Vault's side: it holds even when their Secret is already gone, or
// a revocation failed after it was deleted. A key aisa issued has issued_at; the hashes of a
// consumer that never had a Secret are left as the holder wrote them.
func (k *Keys) revokeDropped(ctx context.Context, valid map[string]consumers.Declaration) error {
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(valid)) {
		if valid[name].Namespace != "" || strings.Contains(name, "/") {
			continue
		}
		rec, err := k.read(ctx, name)
		if err != nil {
			errs = append(errs, fmt.Errorf("consumer %q: %w", name, err))
			continue
		}
		if rec.issuedAt.IsZero() {
			continue
		}
		if err := k.revoke(ctx, name, rec); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func sortedTargets(m map[target][]string) []target {
	ts := slices.Collect(maps.Keys(m))
	slices.SortFunc(ts, func(a, b target) int {
		return strings.Compare(a.namespace+"/"+a.name, b.namespace+"/"+b.name)
	})
	return ts
}

// reconcileConsumer brings one consumer's Secret and hashes in step
// (KUBERNETES_OBJECTS.md, the table of states).
func (k *Keys) reconcileConsumer(ctx context.Context, name string, t target) (string, error) {
	if strings.Contains(name, "/") {
		// Vault would put it under a folder, where the decision API does not look.
		return StateError, errors.New("a consumer with a Secret may not have '/' in its name")
	}
	rec, err := k.read(ctx, name)
	if err != nil {
		return StateError, err
	}
	secret, err := k.k8s.CoreV1().Secrets(t.namespace).Get(ctx, t.name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		secret = nil
	case err != nil:
		return StateError, fmt.Errorf("get the Secret: %w", err)
	case !k.owns(secret, name):
		// Never adopted, overwritten or deleted.
		return StateNotOwned, fmt.Errorf("the Secret %s/%s exists and is not this consumer's (label %s)",
			t.namespace, t.name, LabelConsumer)
	}

	now := k.opts.Now()
	current := ""
	if secret != nil {
		if key := string(secret.Data[SecretKey]); key != "" {
			current = hashKey(key)
		}
	}
	changed := rec.clean(now, current, k.opts.Unwritten, k.opts.Grace)

	if current != "" && slices.Contains(rec.hashes, current) {
		return k.keep(ctx, name, t, rec, secret, current, changed)
	}

	reason := IssueMissing
	switch {
	case secret != nil:
		// Never accepted: the key was not issued by aisa, or was edited by hand.
		reason = IssueUnknownKey
	case rec.version == 0:
		reason = IssueNew
	}
	if err := k.issue(ctx, name, t, rec, secret, reason); err != nil {
		return StateError, err
	}
	return StateOK, nil
}

// keep handles a Secret that holds a key aisa issued: it clears the key's marks, restores what
// else was changed, and replaces the key when it is due.
func (k *Keys) keep(ctx context.Context, name string, t target, rec *record, secret *corev1.Secret, current string, changed bool) (string, error) {
	now := k.opts.Now()
	if _, ok := rec.unwritten[current]; ok {
		delete(rec.unwritten, current)
		changed = true
	}
	if _, ok := rec.superseded[current]; ok {
		delete(rec.superseded, current)
		changed = true
	}
	if rec.issuedAt.IsZero() {
		// A key from before aisa recorded when it issued one: its age starts now.
		rec.issuedAt = now
		changed = true
	}
	if changed {
		if err := k.write(ctx, name, rec); err != nil {
			return StateError, err
		}
	}
	secret, err := k.restore(ctx, secret, name)
	if err != nil {
		return StateError, err
	}
	if k.opts.Rotation > 0 && now.Sub(rec.issuedAt) >= k.opts.Rotation {
		if err := k.issue(ctx, name, t, rec, secret, IssueRotation); err != nil {
			return StateError, err
		}
	}
	return StateOK, nil
}

// issue gives the consumer a new key in three steps, each of which a later pass can finish:
// the hash becomes valid in Vault with a mark, the key is written to the Secret, the mark is
// cleared. The hashes the consumer had are superseded and stay valid for the grace period.
func (k *Keys) issue(ctx context.Context, name string, t target, rec *record, secret *corev1.Secret, reason string) error {
	if r, ok := k.backoff[name]; ok && k.opts.Now().Before(r.at) {
		return fmt.Errorf("the last attempt to write the Secret failed; the next one is at %s", r.at.UTC().Format(time.RFC3339))
	}
	key, err := newKey(k.opts.Rand)
	if err != nil {
		return err
	}
	hash := hashKey(key)
	now := k.opts.Now()
	for _, old := range rec.hashes {
		if _, ok := rec.superseded[old]; !ok {
			rec.superseded[old] = now
		}
	}
	rec.hashes = append(rec.hashes, hash)
	rec.unwritten[hash] = now
	rec.issuedAt = now
	if err := k.write(ctx, name, rec); err != nil {
		return fmt.Errorf("make the new key valid: %w", err)
	}
	if err := k.writeSecret(ctx, name, t, secret, key); err != nil {
		wait := min(max(k.backoff[name].wait*2, minRetry), maxRetry)
		k.backoff[name] = retry{at: k.opts.Now().Add(wait), wait: wait}
		return fmt.Errorf("write the new key to the Secret (next attempt in %s): %w", wait, err)
	}
	delete(k.backoff, name)
	delete(rec.unwritten, hash)
	if err := k.write(ctx, name, rec); err != nil {
		// The next pass clears the mark: the Secret holds the key.
		return fmt.Errorf("clear the new key's mark: %w", err)
	}
	k.log.Info("issued a key for the consumer", "consumer", name, "reason", reason,
		"namespace", t.namespace, "secret", t.name)
	if k.opts.OnIssue != nil {
		k.opts.OnIssue(reason)
	}
	return nil
}

// removeStale deletes the Secrets of this install whose consumer no longer names them, and
// the hashes of consumers no longer declared. A consumer whose declaration cannot be read is
// left as it is: aisa cannot tell what it names.
func (k *Keys) removeStale(ctx context.Context, valid map[string]consumers.Declaration, invalid map[string]bool) error {
	selector := labels.Set{LabelManagedBy: ManagedBy, LabelInstall: k.opts.Install}.String() + "," + LabelConsumer
	list, err := k.k8s.CoreV1().Secrets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("list the consumers' Secrets: %w", err)
	}
	var errs []error
	for i := range list.Items {
		s := &list.Items[i]
		name := s.Annotations[AnnotationConsumer]
		if name == "" || !k.owns(s, name) || invalid[name] {
			continue
		}
		decl, declared := valid[name]
		if declared && decl.Namespace == s.Namespace && decl.Secret == s.Name {
			continue
		}
		// The hashes go first: when that fails, the Secret stays, and the next pass tries again.
		// A consumer moved to another Secret keeps them: the next pass issues a key there and
		// supersedes these.
		if !declared || decl.Namespace == "" {
			rec, err := k.read(ctx, name)
			if err == nil {
				err = k.revoke(ctx, name, rec)
			}
			if err != nil {
				errs = append(errs, err)
				continue
			}
		}
		err := k.k8s.CoreV1().Secrets(s.Namespace).Delete(ctx, s.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &s.UID},
		})
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete the Secret %s/%s of consumer %q: %w", s.Namespace, s.Name, name, err))
			continue
		}
		k.log.Info("deleted the Secret of a consumer that no longer names it", "consumer", name,
			"namespace", s.Namespace, "secret", s.Name)
	}
	return errors.Join(errs...)
}

// revoke removes the hashes of a consumer that is no longer declared, or no longer names a
// Secret.
func (k *Keys) revoke(ctx context.Context, name string, rec *record) error {
	if len(rec.hashes) == 0 && len(rec.unwritten) == 0 && len(rec.superseded) == 0 && rec.issuedAt.IsZero() {
		return nil
	}
	rec.hashes = nil
	clear(rec.unwritten)
	clear(rec.superseded)
	rec.issuedAt = time.Time{}
	if err := k.write(ctx, name, rec); err != nil {
		return fmt.Errorf("remove the hashes of consumer %q: %w", name, err)
	}
	k.log.Info("removed the key hashes of a consumer that no longer names a Secret", "consumer", name)
	return nil
}

// owns reports whether a Secret is this install's Secret of the consumer.
func (k *Keys) owns(s *corev1.Secret, name string) bool {
	return s.Labels[LabelManagedBy] == ManagedBy && s.Labels[LabelInstall] == k.opts.Install &&
		s.Labels[LabelConsumer] == DerivedName(name, "consumer") && s.Annotations[AnnotationConsumer] == name
}

// restore puts back what aisa manages in a Secret it owns, other than the key: the base URL. It
// returns the Secret as it is now.
func (k *Keys) restore(ctx context.Context, s *corev1.Secret, name string) (*corev1.Secret, error) {
	if string(s.Data[SecretBaseURL]) == k.opts.BaseURL {
		return s, nil
	}
	next := s.DeepCopy()
	next.Data[SecretBaseURL] = []byte(k.opts.BaseURL)
	updated, err := k.k8s.CoreV1().Secrets(s.Namespace).Update(ctx, next, metav1.UpdateOptions{})
	if err != nil {
		return s, fmt.Errorf("restore the Secret's %s: %w", SecretBaseURL, err)
	}
	k.log.Info("restored the consumer's Secret", "consumer", name, "namespace", s.Namespace, "secret", s.Name)
	return updated, nil
}

// writeSecret creates the Secret, or updates the one aisa owns, with the key. An update is
// made on the version read, so it fails rather than undo a change it did not see.
func (k *Keys) writeSecret(ctx context.Context, name string, t target, existing *corev1.Secret, key string) error {
	meta := metav1.ObjectMeta{
		Name:      t.name,
		Namespace: t.namespace,
		Labels: map[string]string{
			LabelManagedBy: ManagedBy,
			LabelInstall:   k.opts.Install,
			LabelConsumer:  DerivedName(name, "consumer"),
		},
		Annotations: map[string]string{AnnotationConsumer: name},
	}
	data := map[string][]byte{SecretBaseURL: []byte(k.opts.BaseURL), SecretKey: []byte(key)}
	secrets := k.k8s.CoreV1().Secrets(t.namespace)
	if existing == nil {
		_, err := secrets.Create(ctx, &corev1.Secret{ObjectMeta: meta, Type: corev1.SecretTypeOpaque, Data: data}, metav1.CreateOptions{})
		return err
	}
	s := existing.DeepCopy()
	maps.Copy(s.Labels, meta.Labels)
	if s.Annotations == nil {
		s.Annotations = map[string]string{}
	}
	maps.Copy(s.Annotations, meta.Annotations)
	if s.Data == nil {
		s.Data = map[string][]byte{}
	}
	maps.Copy(s.Data, data)
	_, err := secrets.Update(ctx, s, metav1.UpdateOptions{})
	return err
}

// record is a consumer's secret in Vault, as aisa reads and writes it. Fields it does not know
// are kept as they are.
type record struct {
	data       map[string]any
	version    int
	hashes     []string
	unwritten  map[string]time.Time
	superseded map[string]time.Time
	issuedAt   time.Time
}

func (k *Keys) path(name string) string { return k.opts.Prefix + "/consumers/" + name }

func (k *Keys) read(ctx context.Context, name string) (*record, error) {
	data, version, err := k.kv.ReadVersion(ctx, k.opts.Mount, k.path(name))
	switch {
	case errors.Is(err, vault.ErrNotFound):
		// A secret whose current version is deleted keeps that version for check-and-set.
		data = map[string]any{}
	case err != nil:
		return nil, fmt.Errorf("read the consumer's hashes: %w", err)
	}
	s, _ := data[fieldHashes].(string)
	issued, _ := data[fieldIssuedAt].(string)
	r := &record{
		data:       data,
		version:    version,
		hashes:     splitHashes(s),
		unwritten:  readMarks(data[fieldUnwritten]),
		superseded: readMarks(data[fieldSuperseded]),
	}
	r.issuedAt, _ = time.Parse(time.RFC3339, issued)
	return r, nil
}

// write writes the record on the version it was read at, and advances that version.
func (k *Keys) write(ctx context.Context, name string, r *record) error {
	data := maps.Clone(r.data)
	data[fieldHashes] = strings.Join(r.hashes, ",")
	setMarks(data, fieldUnwritten, r.unwritten)
	setMarks(data, fieldSuperseded, r.superseded)
	if r.issuedAt.IsZero() {
		delete(data, fieldIssuedAt)
	} else {
		data[fieldIssuedAt] = r.issuedAt.UTC().Format(time.RFC3339)
	}
	if err := k.kv.Write(ctx, k.opts.Mount, k.path(name), data, r.version); err != nil {
		return fmt.Errorf("write the consumer's hashes: %w", err)
	}
	r.data = data
	r.version++
	return nil
}

// clean removes the hashes whose time is over: a mark of a key that was never written to the
// Secret, after unwritten, and a superseded key, after the grace period. current, the hash of
// the key in the Secret, is never removed. It reports whether it changed anything.
func (r *record) clean(now time.Time, current string, unwritten, grace time.Duration) bool {
	changed := false
	drop := func(hash string) {
		r.hashes = slices.DeleteFunc(r.hashes, func(h string) bool { return h == hash })
		delete(r.unwritten, hash)
		delete(r.superseded, hash)
		changed = true
	}
	for hash, at := range r.unwritten {
		if hash != current && now.Sub(at) > unwritten {
			drop(hash)
		}
	}
	for hash, at := range r.superseded {
		if hash != current && now.Sub(at) > grace {
			drop(hash)
		}
	}
	// A mark of a hash that is no longer valid is left over from an edit by hand.
	for _, marks := range []map[string]time.Time{r.unwritten, r.superseded} {
		for hash := range marks {
			if !slices.Contains(r.hashes, hash) {
				delete(marks, hash)
				changed = true
			}
		}
	}
	return changed
}

func readMarks(v any) map[string]time.Time {
	marks := map[string]time.Time{}
	m, _ := v.(map[string]any)
	for hash, at := range m {
		s, _ := at.(string)
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			marks[strings.ToLower(hash)] = t
		}
	}
	return marks
}

func setMarks(data map[string]any, field string, marks map[string]time.Time) {
	if len(marks) == 0 {
		delete(data, field)
		return
	}
	m := make(map[string]any, len(marks))
	for hash, at := range marks {
		m[hash] = at.UTC().Format(time.RFC3339)
	}
	data[field] = m
}

// splitHashes reads key_sha256 as the decision API does: hashes separated by commas or white
// space, in lower case. Values that are not SHA-256 hex digests are dropped.
func splitHashes(v string) []string {
	var hashes []string
	for f := range strings.FieldsFuncSeq(v, func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) {
		f = strings.ToLower(f)
		if b, err := hex.DecodeString(f); err == nil && len(b) == sha256.Size && !slices.Contains(hashes, f) {
			hashes = append(hashes, f)
		}
	}
	return hashes
}

func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// newKey returns a key of 32 random bytes, hex-encoded, with a prefix that tells it from a
// provider's key.
func newKey(r io.Reader) (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", fmt.Errorf("generate a key: %w", err)
	}
	return "aisa-" + hex.EncodeToString(b), nil
}

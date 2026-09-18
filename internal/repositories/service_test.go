package repositories

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iamangus/code-mcp/internal/github"
	"github.com/iamangus/code-mcp/internal/repositorycatalog"
)

type fakeManager struct {
	root      string
	syncURLs  []string
	syncNames []string
	err       error
}

func (m *fakeManager) SyncRepo(url, name string, _ ...string) error {
	m.syncURLs = append(m.syncURLs, url)
	m.syncNames = append(m.syncNames, name)
	return m.err
}

func (m *fakeManager) RepoDir(name string) string { return filepath.Join(m.root, name) }

func (m *fakeManager) CreateFoundationBranch(_, _, _ string) (string, string, error) {
	return filepath.Join(m.root, "foundation"), "foundation-sha", m.err
}

type fakeMetadataReader struct{ metadata repositorycatalog.GitMetadata }

func (r fakeMetadataReader) ReadGitMetadata(context.Context, string) (repositorycatalog.GitMetadata, error) {
	return r.metadata, nil
}

type fakeGitHub struct {
	repositories map[string]*github.Repository
	getCalls     int
	createCalls  int
	forkCalls    int
	createErr    error
	forkErr      error
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{repositories: make(map[string]*github.Repository)}
}

func (f *fakeGitHub) ListOwnedRepositories(context.Context) ([]github.Repository, error) {
	return nil, nil
}

func (f *fakeGitHub) GetRepository(_ context.Context, name string) (*github.Repository, error) {
	f.getCalls++
	repo, ok := f.repositories[name]
	if !ok {
		return nil, github.ErrNotFound
	}
	return repo, nil
}

func (f *fakeGitHub) CreateRepository(_ context.Context, name, _ string, private bool) (*github.Repository, error) {
	f.createCalls++
	repo := &github.Repository{Name: name, FullName: "acme/" + name, CloneURL: "https://github.com/acme/" + name + ".git", DefaultBranch: "main", Private: private}
	f.repositories[name] = repo
	if f.createErr != nil {
		return nil, f.createErr
	}
	return repo, nil
}

func (f *fakeGitHub) ForkPublicRepository(_ context.Context, owner, upstream, name string, private bool) (*github.Repository, error) {
	f.forkCalls++
	repo := &github.Repository{Name: name, FullName: "acme/" + name, CloneURL: "https://github.com/acme/" + name + ".git", DefaultBranch: "main", Private: private, Fork: true, Parent: &github.Repository{FullName: owner + "/" + upstream}}
	f.repositories[name] = repo
	if f.forkErr != nil {
		return nil, f.forkErr
	}
	return repo, nil
}

func (f *fakeGitHub) CreatePR(context.Context, github.CreatePROptions) (*github.PR, error) {
	return nil, errors.New("not implemented: CREATEPR_MARKER")
}
func (f *fakeGitHub) FindPR(context.Context, string, string) (*github.PR, error) {
	return nil, github.ErrNotFound
}
func (f *fakeGitHub) UpdatePR(context.Context, string, int, string, string) error {
	return errors.New("not implemented: OTHER")
}
func (f *fakeGitHub) PromotePR(context.Context, string, int) error {
	return errors.New("not implemented: OTHER")
}
func (f *fakeGitHub) GetPR(context.Context, string, int) (*github.PR, error) {
	return nil, errors.New("not implemented: GETPR")
}
func (f *fakeGitHub) GetPRChecks(context.Context, string, string) (*github.PRChecks, error) {
	return nil, errors.New("not implemented: GETPRCHECKS")
}
func (f *fakeGitHub) MergePR(context.Context, string, int) error {
	return errors.New("not implemented: OTHER")
}

func newService(t *testing.T, client *fakeGitHub) (*Service, *fakeManager, *repositorycatalog.Catalog) {
	t.Helper()
	manager := &fakeManager{root: filepath.Join(t.TempDir(), "repos")}
	catalog, err := repositorycatalog.New(t.TempDir(), fakeMetadataReader{metadata: repositorycatalog.GitMetadata{OriginURL: "https://github.com/acme/widget.git", HeadSHA: "abc123"}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(manager, catalog, client)
	if err != nil {
		t.Fatal(err)
	}
	return service, manager, catalog
}

func TestProvisionPrivateCreatesSyncsAndCatalogs(t *testing.T) {
	client := newFakeGitHub()
	service, manager, catalog := newService(t, client)

	record, err := service.ProvisionPrivate(context.Background(), " Widget ", "a private widget")
	if err != nil {
		t.Fatal(err)
	}
	if client.createCalls != 1 || len(manager.syncURLs) != 1 || manager.syncURLs[0] != "https://github.com/acme/widget.git" {
		t.Fatalf("create=%d sync URLs=%v", client.createCalls, manager.syncURLs)
	}
	if record.Name != "widget" || record.Path != manager.RepoDir("widget") || record.DefaultBranch != "main" {
		t.Fatalf("record = %+v", record)
	}
	persisted, err := catalog.Get("widget")
	if err != nil || persisted == nil || persisted.HeadSHA != "abc123" {
		t.Fatalf("persisted record = %+v, %v", persisted, err)
	}
}

func TestProvisionPrivateExistingRepositoryDoesNotCreateAgain(t *testing.T) {
	client := newFakeGitHub()
	client.repositories["widget"] = &github.Repository{Name: "widget", CloneURL: "https://github.com/acme/widget.git", DefaultBranch: "trunk", Private: true}
	service, manager, _ := newService(t, client)

	if _, err := service.ProvisionPrivate(context.Background(), "widget", "ignored"); err != nil {
		t.Fatal(err)
	}
	if client.createCalls != 0 || len(manager.syncURLs) != 1 {
		t.Fatalf("create=%d sync calls=%d", client.createCalls, len(manager.syncURLs))
	}
}

func TestProvisionPrivateRechecksAfterAmbiguousCreateFailure(t *testing.T) {
	client := newFakeGitHub()
	client.createErr = errors.New("connection dropped")
	service, manager, _ := newService(t, client)

	if _, err := service.ProvisionPrivate(context.Background(), "widget", "description"); err != nil {
		t.Fatal(err)
	}
	if client.createCalls != 1 || client.getCalls != 2 || len(manager.syncURLs) != 1 {
		t.Fatalf("create=%d get=%d sync=%d", client.createCalls, client.getCalls, len(manager.syncURLs))
	}
}

func TestForkPublicOnlyForksWhenTargetIsAbsent(t *testing.T) {
	client := newFakeGitHub()
	service, manager, _ := newService(t, client)

	if _, err := service.ForkPublic(context.Background(), "upstream", "source", "fork"); err != nil {
		t.Fatal(err)
	}
	if client.forkCalls != 1 || len(manager.syncURLs) != 1 {
		t.Fatalf("fork=%d sync=%d", client.forkCalls, len(manager.syncURLs))
	}
	if _, err := service.ForkPublic(context.Background(), "upstream", "source", "fork"); err != nil {
		t.Fatal(err)
	}
	if client.forkCalls != 1 || len(manager.syncURLs) != 2 {
		t.Fatalf("fork=%d sync=%d", client.forkCalls, len(manager.syncURLs))
	}
}

func TestForkPublicRechecksAfterAmbiguousForkFailure(t *testing.T) {
	client := newFakeGitHub()
	client.forkErr = errors.New("connection dropped")
	service, manager, _ := newService(t, client)

	if _, err := service.ForkPublic(context.Background(), "upstream", "source", "fork"); err != nil {
		t.Fatal(err)
	}
	if client.forkCalls != 1 || client.getCalls != 2 || len(manager.syncURLs) != 1 {
		t.Fatalf("fork=%d get=%d sync=%d", client.forkCalls, client.getCalls, len(manager.syncURLs))
	}
}

func TestLookupDoesNotSyncStaleCatalogEntry(t *testing.T) {
	client := newFakeGitHub()
	service, manager, catalog := newService(t, client)
	if _, err := catalog.Save(repositorycatalog.Record{Name: "missing", Path: manager.RepoDir("missing")}); err != nil {
		t.Fatal(err)
	}

	record, err := service.Lookup(context.Background(), "missing")
	if err != nil || record != nil || len(manager.syncURLs) != 0 {
		t.Fatalf("Lookup = %+v, %v; sync=%v", record, err, manager.syncURLs)
	}
}

type foundationGitHub struct {
	fakeGitHub
	pr          *github.PR
	ensureErr   error
	ensureCalls int
	findResults []error
	findCalls   int
	createCalls int
}

func (f *foundationGitHub) CreatePR(context.Context, github.CreatePROptions) (*github.PR, error) {
	f.createCalls++
	return f.pr, nil
}

func (f *foundationGitHub) FindPR(context.Context, string, string) (*github.PR, error) {
	if f.findCalls >= len(f.findResults) {
		return nil, github.ErrNotFound
	}
	err := f.findResults[f.findCalls]
	f.findCalls++
	if err != nil {
		return nil, err
	}
	return f.pr, nil
}

func (f *foundationGitHub) EnsureRequiredCheck(context.Context, string, string, string) error {
	f.ensureCalls++
	return f.ensureErr
}

func newFoundationService(t *testing.T, client *foundationGitHub) (*Service, *fakeManager, *repositorycatalog.Catalog) {
	t.Helper()
	manager := &fakeManager{root: filepath.Join(t.TempDir(), "repos")}
	catalog, err := repositorycatalog.New(t.TempDir(), fakeMetadataReader{metadata: repositorycatalog.GitMetadata{OriginURL: "https://github.com/acme/drill.git", HeadSHA: "abc123"}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(manager, catalog, client)
	if err != nil {
		t.Fatal(err)
	}
	return service, manager, catalog
}

func TestEnsureFoundationContinuesWhenProtectionIsPlanLimited(t *testing.T) {
	client := &foundationGitHub{fakeGitHub: *newFakeGitHub(), pr: &github.PR{Number: 7, HTMLURL: "https://github.com/acme/drill/pull/7"}}
	client.repositories["drill"] = &github.Repository{Name: "drill", FullName: "acme/drill", CloneURL: "https://github.com/acme/drill.git", DefaultBranch: "main", Private: true}
	client.ensureErr = github.ErrPlanLimited
	service, _, catalog := newFoundationService(t, client)

	record, err := service.EnsureFoundation(context.Background(), "drill")
	if !errors.Is(err, ErrFoundationPending) {
		t.Fatalf("EnsureFoundation = %v, want ErrFoundationPending", err)
	}
	if client.ensureCalls != 1 || record.Foundation == nil || record.Foundation.Status != "pending" {
		t.Fatalf("ensure=%d foundation=%+v", client.ensureCalls, record.Foundation)
	}
	if _, err := catalog.Get("drill"); err != nil {
		t.Fatalf("catalog persist: %v", err)
	}
}

func TestEnsureFoundationRetriesAdoptExistingFoundationPR(t *testing.T) {
	client := &foundationGitHub{fakeGitHub: *newFakeGitHub(), pr: &github.PR{Number: 7, HTMLURL: "https://github.com/acme/drill/pull/7"}}
	client.repositories["drill"] = &github.Repository{Name: "drill", FullName: "acme/drill", CloneURL: "https://github.com/acme/drill.git", DefaultBranch: "main", Private: true}
	client.ensureErr = github.ErrPlanLimited
	client.findResults = []error{nil}
	service, _, catalog := newFoundationService(t, client)

	record, err := service.EnsureFoundation(context.Background(), "drill")
	if !errors.Is(err, ErrFoundationPending) {
		t.Fatalf("EnsureFoundation = %v, want ErrFoundationPending", err)
	}
	if client.createCalls != 0 || client.findCalls != 1 || client.ensureCalls != 1 {
		t.Fatalf("create=%d find=%d ensure=%d", client.createCalls, client.findCalls, client.ensureCalls)
	}
	if record.Foundation == nil || record.Foundation.PRNumber != 7 || record.Foundation.Status != "pending" {
		t.Fatalf("foundation = %+v", record.Foundation)
	}
	if _, err := catalog.Get("drill"); err != nil {
		t.Fatalf("catalog persist: %v", err)
	}
}

func TestEnsureFoundationFailsOnUnexpectedProtectionError(t *testing.T) {
	client := &foundationGitHub{fakeGitHub: *newFakeGitHub(), pr: &github.PR{Number: 7, HTMLURL: "https://github.com/acme/drill/pull/7"}}
	client.repositories["drill"] = &github.Repository{Name: "drill", FullName: "acme/drill", CloneURL: "https://github.com/acme/drill.git", DefaultBranch: "main", Private: true}
	client.ensureErr = errors.New("connection dropped")
	service, _, _ := newFoundationService(t, client)

	if _, err := service.EnsureFoundation(context.Background(), "drill"); err == nil || !strings.Contains(err.Error(), "configure foundation required check") {
		t.Fatalf("EnsureFoundation = %v, want configure failure", err)
	}
}

// Package testenv starts an in-process sg-server for tests.
package testenv

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	sgitv1 "github.com/hermes98761234/sgit/gen/sgit/v1"
	"github.com/hermes98761234/sgit/internal/auth"
	"github.com/hermes98761234/sgit/internal/gitx"
	"github.com/hermes98761234/sgit/internal/rpc"
	"github.com/hermes98761234/sgit/internal/server"
	"github.com/hermes98761234/sgit/internal/store"
	"google.golang.org/grpc"
)

type Env struct {
	t          testing.TB
	Addr       string
	AdminToken string
	DataDir    string
	Store      *store.Store
	Server     *server.Server
}

// Must returns v, or panics (failing the calling test) if err is non-nil.
// Go cannot forward a multi-value call together with a leading t argument, so
// Must takes the (value, error) pair directly: Must(f()).
func Must[T any](v T, err error) T {
	if err != nil {
		panic(fmt.Sprintf("testenv.Must: unexpected error: %v", err))
	}
	return v
}

func Start(t testing.TB, opts ...func(*server.Config)) *Env {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st := Must(store.Open(filepath.Join(dir, "sg.db")))
	t.Cleanup(func() { st.Close() }) // cleanups run LIFO: g.Stop, srv.Close, st.Close
	admin := Must(st.CreateUser(ctx, "admin", "admin@sgit.local", true))
	id, tok, hash, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateToken(ctx, admin.ID, id, hash); err != nil {
		t.Fatal(err)
	}
	cfg := server.Config{DataDir: dir}
	for _, o := range opts {
		o(&cfg)
	}
	srv := Must(server.New(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)
	g := server.NewGRPC(st)
	srv.Register(g)
	lis := Must(net.Listen("tcp", "127.0.0.1:0"))
	go g.Serve(lis)
	t.Cleanup(g.Stop)
	return &Env{t: t, Addr: lis.Addr().String(), AdminToken: tok, DataDir: dir, Store: st, Server: srv}
}

func (e *Env) Conn(token string) *grpc.ClientConn {
	conn := Must(grpc.NewClient(e.Addr, rpc.DialOptions(token, nil)...))
	e.t.Cleanup(func() { conn.Close() })
	return conn
}

func (e *Env) Project(token string) sgitv1.ProjectClient {
	return sgitv1.NewProjectClient(e.Conn(token))
}
func (e *Env) Admin(token string) sgitv1.AdminClient { return sgitv1.NewAdminClient(e.Conn(token)) }

func (e *Env) CreateRepo(name string) string {
	e.t.Helper()
	return Must(e.Admin(e.AdminToken).CreateRepo(context.Background(), &sgitv1.CreateRepoRequest{Name: name})).Main
}

// NewUser creates a user and returns its token. If level is "read" or
// "write", it is granted on project.
func (e *Env) NewUser(name, project, level string) string {
	e.t.Helper()
	ctx := context.Background()
	adm := e.Admin(e.AdminToken)
	tok := Must(adm.CreateToken(ctx, &sgitv1.CreateTokenRequest{User: name}))
	if level != "" {
		Must(adm.GrantACL(ctx, &sgitv1.GrantACLRequest{User: name, Project: project, Level: level}))
	}
	return tok.Token
}

// CreateRepoProject creates repo name and project name mounting the whole
// repo at ".", and returns the project head (equal to the repo's main).
func (e *Env) CreateRepoProject(name string) string {
	e.t.Helper()
	e.CreateRepo(name)
	e.CreateProject(name)
	return e.Mount(name, ".", name, ".", false)
}

// ProjectID returns the id of the project ref names.
func (e *Env) ProjectID(ref string) int64 {
	e.t.Helper()
	return Must(e.Admin(e.AdminToken).GetProject(context.Background(), &sgitv1.GetProjectRequest{Project: ref})).Id
}

func (e *Env) Upload(token, project string, blobs ...[]byte) []string {
	e.t.Helper()
	return Must(rpc.UploadBlobs(context.Background(), e.Project(token), project, blobs))
}

// Git opens the repo's bare git dir directly (caller closes).
func (e *Env) Git(repo string) *gitx.Repo {
	return gitx.Open(filepath.Join(e.DataDir, "repos", repo+".git"))
}

// CreateProject creates an empty project and returns its id.
func (e *Env) CreateProject(name string) int64 {
	e.t.Helper()
	return Must(e.Admin(e.AdminToken).CreateProject(context.Background(), &sgitv1.CreateProjectRequest{Name: name})).Id
}

// Mount mounts repo:folder at path in project and returns the project head.
func (e *Env) Mount(project, path, repo, folder string, readOnly bool) string {
	e.t.Helper()
	return Must(e.Admin(e.AdminToken).MountProject(context.Background(), &sgitv1.MountProjectRequest{
		Project: project, Path: path, Repo: repo, Folder: folder, ReadOnly: readOnly})).Head
}

// ProjectGit opens a project's derived repo directly (caller closes).
func (e *Env) ProjectGit(id int64) *gitx.Repo {
	return gitx.Open(filepath.Join(e.DataDir, "projects", strconv.FormatInt(id, 10)+".git"))
}

// Files returns path -> content of every file at rev.
func (e *Env) Files(repo, rev string) map[string]string {
	e.t.Helper()
	ctx := context.Background()
	r := e.Git(repo)
	defer r.Close()
	out := map[string]string{}
	for p, fe := range Must(r.ListFiles(ctx, rev)) {
		out[p] = string(Must(r.ReadObject(ctx, fe.SHA)).Data)
	}
	return out
}

// ProjectFiles returns path -> content of every file at rev of a project's
// derived repo (where drafts live).
func (e *Env) ProjectFiles(project, rev string) map[string]string {
	e.t.Helper()
	ctx := context.Background()
	r := e.ProjectGit(e.ProjectID(project))
	defer r.Close()
	out := map[string]string{}
	for p, fe := range Must(r.ListFiles(ctx, rev)) {
		out[p] = string(Must(r.ReadObject(ctx, fe.SHA)).Data)
	}
	return out
}

// CommitDirect commits files on top of main through gitx, bypassing the server
// (simulates an external writer). Safe to call from a BeforeCAS hook.
func (e *Env) CommitDirect(repo string, files map[string]string, msg string) string {
	ctx := context.Background()
	r := e.Git(repo)
	defer r.Close()
	main := Must(r.ReadRef(ctx, gitx.MainRef))
	var ch []gitx.Change
	for p, content := range files {
		ch = append(ch, gitx.Change{Path: p, Op: gitx.OpAdd, BlobSHA: Must(r.WriteBlob(ctx, []byte(content)))})
	}
	tree := Must(r.BuildTree(ctx, main, ch))
	sig := gitx.Signature{Name: "direct", Email: "direct@sgit.local", When: time.Now()}
	c := Must(r.CommitTree(ctx, tree, []string{main}, msg, sig, sig))
	if err := r.UpdateRef(ctx, gitx.MainRef, c, main); err != nil {
		e.t.Fatalf("CommitDirect: %v", err)
	}
	return c
}

func Add(path, sha string) *sgitv1.Change {
	return &sgitv1.Change{Path: path, Op: sgitv1.ChangeOp_CHANGE_OP_ADD, BlobSha: sha}
}

func Modify(path, sha string) *sgitv1.Change {
	return &sgitv1.Change{Path: path, Op: sgitv1.ChangeOp_CHANGE_OP_MODIFY, BlobSha: sha}
}

func Delete(path string) *sgitv1.Change {
	return &sgitv1.Change{Path: path, Op: sgitv1.ChangeOp_CHANGE_OP_DELETE}
}

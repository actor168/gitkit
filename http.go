package gitkit

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strings"
	"syscall"
)

type service struct {
	method  string
	suffix  string
	handler func(string, http.ResponseWriter, *Request)
	rpc     string
}

type Server struct {
	config         Config
	services       []service
	AuthFunc       func(Credential, *Request) (bool, error)
	FilterRepoFunc func([]string, *Request) []string
}

type Request struct {
	*http.Request
	RepoName string
	RepoPath string
}

type KitResponse struct {
	Code int         `json:"code"`
	Data interface{} `json:"data"`
}

type KitRepoResponse struct {
	RepoPath string `json:"repoPath"`
}

type KitListRepoResponse struct {
	RepoPath []string `json:"repoPath"`
}

func New(cfg Config) *Server {
	s := Server{config: cfg}
	s.services = []service{
		{"GET", "/info/refs", s.getInfoRefs, ""},
		{"POST", "/git-upload-pack", s.postRPC, "git-upload-pack"},
		{"POST", "/git-receive-pack", s.postRPC, "git-receive-pack"},
		{"GET", "/repos", s.listRepo, ""},
		{"POST", "/repo", s.createRepo, ""},
		{"DELETE", "/repo", s.deleteRepo, ""},
	}

	// Use PATH if full path is not specified
	if s.config.GitPath == "" {
		s.config.GitPath = "git"
	}

	return &s
}

// findService returns a matching git subservice and parsed repository name
func (s *Server) findService(req *http.Request) (*service, string) {
	for _, svc := range s.services {
		if svc.method == req.Method && strings.HasSuffix(req.URL.Path, svc.suffix) {
			path := strings.Replace(req.URL.Path, svc.suffix, "", 1)
			return &svc, path
		}
	}
	return nil, ""
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	logInfo("request", r.Method+" "+r.Host+r.URL.String())

	// Find the git subservice to handle the request
	svc, repoUrlPath := s.findService(r)
	if svc == nil {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	// Determine namespace and repo name from request path
	repoNamespace, repoName := getNamespaceAndRepo(repoUrlPath)
	if r.Method == http.MethodGet && strings.HasSuffix(r.RequestURI, "/repos") {
		// skip list repos
	} else if repoName == "" {
		logError("auth", fmt.Errorf("no repo name provided"))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	req := &Request{
		Request:  r,
		RepoName: path.Join(repoNamespace, repoName),
		RepoPath: path.Join(s.config.Dir, repoNamespace, repoName),
	}

	if s.config.Auth {
		if s.AuthFunc == nil {
			logError("auth", fmt.Errorf("no auth backend provided"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			w.Header()["WWW-Authenticate"] = []string{`Basic realm=""`}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		cred, err := getCredential(r)
		if err != nil {
			logError("auth", err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		allow, err := s.AuthFunc(cred, req)
		if !allow || err != nil {
			if err != nil {
				logError("auth", err)
			}

			logError("auth", fmt.Errorf("rejected user %s", cred.Username))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}

	if req.Method == http.MethodPost && strings.HasSuffix(req.RequestURI, "/repo") ||
		req.Method == http.MethodGet && strings.HasSuffix(req.RequestURI, "/repos") {
		// skip create repo
		svc.handler(svc.rpc, w, req)
		return
	}

	if !repoExists(req.RepoPath) && s.config.AutoCreate {
		err := initRepo(req.RepoName, &s.config)
		if err != nil {
			logError("repo-init", err)
		}
	}

	if !repoExists(req.RepoPath) {
		logError("repo-init", fmt.Errorf("%s does not exist", req.RepoPath))
		http.NotFound(w, r)
		return
	}

	svc.handler(svc.rpc, w, req)
}

func (s *Server) getInfoRefs(_ string, w http.ResponseWriter, r *Request) {
	context := "get-info-refs"
	rpc := r.URL.Query().Get("service")

	if !(rpc == "git-upload-pack" || rpc == "git-receive-pack") {
		http.Error(w, "Not Found", 404)
		return
	}

	cmd, pipe := gitCommand(s.config.GitPath, subCommand(rpc), "--stateless-rpc", "--advertise-refs", r.RepoPath)
	if err := cmd.Start(); err != nil {
		fail500(w, context, err)
		return
	}
	defer cleanUpProcessGroup(cmd)

	w.Header().Add("Content-Type", fmt.Sprintf("application/x-%s-advertisement", rpc))
	w.Header().Add("Cache-Control", "no-cache")
	w.WriteHeader(200)

	if err := packLine(w, fmt.Sprintf("# service=%s\n", rpc)); err != nil {
		logError(context, err)
		return
	}

	if err := packFlush(w); err != nil {
		logError(context, err)
		return
	}

	if _, err := io.Copy(w, pipe); err != nil {
		logError(context, err)
		return
	}

	if err := cmd.Wait(); err != nil {
		logError(context, err)
		return
	}
}

func (s *Server) postRPC(rpc string, w http.ResponseWriter, r *Request) {
	context := "post-rpc"
	body := r.Body

	if r.Header.Get("Content-Encoding") == "gzip" {
		var err error
		body, err = gzip.NewReader(r.Body)
		if err != nil {
			fail500(w, context, err)
			return
		}
	}

	cmd, pipe := gitCommand(s.config.GitPath, subCommand(rpc), "--stateless-rpc", r.RepoPath)
	defer pipe.Close()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		fail500(w, context, err)
		return
	}
	defer stdin.Close()

	if err := cmd.Start(); err != nil {
		fail500(w, context, err)
		return
	}
	defer cleanUpProcessGroup(cmd)

	if _, err := io.Copy(stdin, body); err != nil {
		fail500(w, context, err)
		return
	}
	stdin.Close()

	w.Header().Add("Content-Type", fmt.Sprintf("application/x-%s-result", rpc))
	w.Header().Add("Cache-Control", "no-cache")
	w.WriteHeader(200)

	if _, err := io.Copy(newWriteFlusher(w), pipe); err != nil {
		logError(context, err)
		return
	}
	if err := cmd.Wait(); err != nil {
		logError(context, err)
		return
	}
}

func (s *Server) createRepo(_ string, w http.ResponseWriter, req *Request) {
	if !repoExists(req.RepoPath) {
		err := initRepo(req.RepoName, &s.config)
		if err != nil {
			fail500(w, "repo-init", err)
			return
		}

		body := &KitResponse{
			Code: 201,
			Data: KitRepoResponse{
				RepoPath: req.RepoName,
			},
		}
		formatResponse(w, body, http.StatusCreated)
		return
	}
	body := &KitResponse{
		Code: 409,
		Data: KitRepoResponse{
			RepoPath: req.RepoName,
		},
	}
	formatResponse(w, body, http.StatusConflict)
}

func (s *Server) listRepo(_ string, w http.ResponseWriter, r *Request) {
	fullPath := s.config.Dir
	dirs, err := os.ReadDir(fullPath)
	if err != nil {
		fail500(w, "list repo", err)
		return
	}
	repos := make([]string, 0)
	for _, repoDir := range dirs {
		if repoDir.IsDir() {
			subDirs, err := os.ReadDir(repoDir.Name())
			if err != nil {
				fail500(w, "list repo", err)
				return
			}
			for _, d := range subDirs {
				if d.IsDir() && strings.HasSuffix(d.Name(), ".git") {
					repos = append(repos, path.Join(repoDir.Name(), d.Name()))
				}
			}
		}
	}

	repos = s.FilterRepoFunc(repos, r)
	body := &KitResponse{
		Code: 200,
		Data: KitListRepoResponse{
			repos,
		},
	}
	formatResponse(w, body, http.StatusOK)
}

func (s *Server) deleteRepo(_ string, w http.ResponseWriter, r *Request) {
	if r.RepoName == "" {
		body := &KitResponse{
			Code: 400,
			Data: KitRepoResponse{
				r.RepoName,
			},
		}
		formatResponse(w, body, http.StatusBadRequest)
		return
	}
	fullPath := path.Join(s.config.Dir, r.RepoName)
	f, err := os.Lstat(fullPath)
	if err != nil || f == nil {
		fail500(w, "find repo", err)
		return
	}

	err = os.RemoveAll(fullPath)
	if err != nil || f == nil {
		fail500(w, "find repo", err)
		return
	}

	body := &KitResponse{
		Code: 202,
		Data: KitRepoResponse{
			r.RepoName,
		},
	}
	formatResponse(w, body, http.StatusAccepted)
}

func (s *Server) Setup() error {
	return s.config.Setup()
}

func initRepo(name string, config *Config) error {
	fullPath := path.Join(config.Dir, name)

	if err := exec.Command(config.GitPath, "init", "--bare", fullPath).Run(); err != nil {
		return err
	}

	if config.AutoHooks && config.Hooks != nil {
		return config.Hooks.setupInDir(fullPath)
	}

	return nil
}

func repoExists(p string) bool {
	_, err := os.Stat(path.Join(p, "objects"))
	return err == nil
}

func gitCommand(name string, args ...string) (*exec.Cmd, io.ReadCloser) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = os.Environ()

	r, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout

	return cmd, r
}

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Sirupsen/logrus"
	jwt "github.com/dgrijalva/jwt-go"
	"github.com/google/go-github/github"
	"github.com/guregu/kami"
	"github.com/netlify/gotell/comments"
	"github.com/netlify/gotell/conf"
	"github.com/rs/cors"
	"github.com/zenazn/goji/web/mutil"
)

const defaultVersion = "unknown version"

var threadRegexp = regexp.MustCompile(`(\d+)-(\d+)-(.+)`)
var slugify = regexp.MustCompile(`\W`)
var squeeze = regexp.MustCompile(`-+`)
var bearerRegexp = regexp.MustCompile(`^(?:B|b)earer (\S+$)`)

type Server struct {
	handler  http.Handler
	config   *conf.Configuration
	client   *github.Client
	settings *settings
	mutex    sync.Mutex
	version  string
}

func Min(x, y int) int {
	if x < y {
		return x
	}
	return y
}

func (s *Server) postComment(ctx context.Context, w http.ResponseWriter, req *http.Request) {
	entryPath := req.URL.Path

	w.Header().Set("Content-Type", "application/json")

	settings := s.getSettings()
	for _, ip := range settings.BannedIPs {
		if req.RemoteAddr == ip {
			w.Header().Add("X-Banned", "IP-Banned")
			fmt.Fprintln(w, "{}")
			return
		}
	}

	entryData, err := s.entryData(entryPath)
	if err != nil {
		jsonError(w, fmt.Sprintf("Unable to read entry data: %v", err), 400)
		return
	}
	if settings.TimeLimit != 0 && time.Now().Sub(entryData.CreatedAt) > time.Duration(settings.TimeLimit) {
		jsonError(w, "Thread is closed for new comments", 401)
		return
	}

	comment := &comments.RawComment{}
	decoder := json.NewDecoder(req.Body)
	if err := decoder.Decode(comment); err != nil {
		jsonError(w, fmt.Sprintf("Error decoding JSON body: %v", err), 422)
		return
	}

	for _, email := range settings.BannedEmails {
		if strings.Contains(comment.Email, email) || strings.Contains(comment.Body, email) || strings.Contains(comment.URL, email) {
			w.Header().Add("X-Banned", "Email-Banned")
			fmt.Fprintln(w, "{}")
			return
		}
	}

	for _, keyword := range settings.BannedKeywords {
		if strings.Contains(comment.Email, keyword) || strings.Contains(comment.Body, keyword) || strings.Contains(comment.URL, keyword) {
			w.Header().Add("X-Banned", "Keyword-Banned")
			fmt.Fprintln(w, "{}")
			return
		}
	}

	comment.IP = req.RemoteAddr
	comment.Date = time.Now().String()
	comment.ID = fmt.Sprintf("%v", time.Now().UnixNano())
	comment.Verified = s.verify(comment.Email, req)

	parts := strings.Split(s.config.API.Repository, "/")
	matches := threadRegexp.FindStringSubmatch(entryData.Thread)
	dir := matches[1] + "/" + matches[2] + "/" + matches[3]
	firstParagraph := strings.SplitAfterN(strings.ToLower(strings.TrimSpace(comment.Body[0:len(comment.Body)])), "\n", 1)[0]
	name := squeeze.ReplaceAllString(strings.Trim(slugify.ReplaceAllString(firstParagraph[0:Min(50, len(firstParagraph))], "-"), "-"), "-")

	pathname := path.Join(
		s.config.Threads.Source,
		dir,
		fmt.Sprintf("%v-%v.json", (time.Now().UnixNano()/1000000), name),
	)

	content, _ := json.Marshal(comment)
	branch := "master"

	if settings.RequireApproval || comment.IsSuspicious() {
		branch = "comment-" + comment.ID
		master, _, err := s.client.Repositories.GetBranch(ctx, parts[0], parts[1], "master")
		sha := master.Commit.GetSHA()
		refName := "refs/heads/" + branch
		if err != nil {
			jsonError(w, fmt.Sprintf("Failed to write comment: %v", err), 500)
			return
		}

		_, _, err = s.client.Git.CreateRef(ctx, parts[0], parts[1], &github.Reference{
			Ref:    &refName,
			Object: &github.GitObject{SHA: &sha},
		})
		if err != nil {
			jsonError(w, fmt.Sprintf("Failed to create comment branch: %v", err), 500)
			return
		}
		message := firstParagraph
		_, _, err = s.client.Repositories.CreateFile(ctx, parts[0], parts[1], pathname, &github.RepositoryContentFileOptions{
			Message: &message,
			Content: content,
			Branch:  &branch,
		})

		if err != nil {
			jsonError(w, fmt.Sprintf("Failed to write comment: %v", err), 500)
			return
		}

		pr := &github.NewPullRequest{
			Title: &message,
			Head:  &branch,
			Base:  master.Name,
		}
		_, _, err = s.client.PullRequests.Create(ctx, parts[0], parts[1], pr)
		if err != nil {
			jsonError(w, fmt.Sprintf("Failed to create PR: %v", err), 500)
			return
		}
	} else {
		message := firstParagraph
		_, _, err = s.client.Repositories.CreateFile(ctx, parts[0], parts[1], pathname, &github.RepositoryContentFileOptions{
			Message: &message,
			Content: content,
			Branch:  &branch,
		})

		if err != nil {
			jsonError(w, fmt.Sprintf("Failed to write comment: %v", err), 500)
			return
		}
	}

	parsedComment := comments.ParseRaw(comment)
	response, _ := json.Marshal(parsedComment)
	w.Write(response)
}

func (s *Server) verify(email string, r *http.Request) bool {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		logrus.Info("No auth header")
		return false
	}

	matches := bearerRegexp.FindStringSubmatch(authHeader)
	if len(matches) != 2 {
		logrus.Info("Not a bearer auth header")
		return false
	}

	token, err := jwt.Parse(matches[1], func(token *jwt.Token) (interface{}, error) {
		if token.Method.Alg() != jwt.SigningMethodHS256.Name {
			return nil, fmt.Errorf("Unexpected signing method: %v", token.Method.Alg())
		}
		return []byte(s.config.JWT.Secret), nil
	})
	if err != nil {
		logrus.Errorf("Error verifying JWT: %v", err)
		return false
	}

	if claims, ok := token.Claims.(jwt.MapClaims); ok && token.Valid {
		claimedEmail, ok := claims["email"]
		logrus.Infof("Checking email %v from claims %v against %v", claimedEmail, claims, email)
		return ok && claimedEmail == email
	}

	return false
}

// Index endpoint
func (s *Server) index(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	sendJSON(w, 200, map[string]string{
		"version":     s.version,
		"name":        "GoTell",
		"description": "GoTell is an API and build tool for handling large amounts of comments for JAMstack products",
	})
}

// ListenAndServe starts the Comments Server
func (s *Server) ListenAndServe() error {
	l := fmt.Sprintf("%v:%v", s.config.API.Host, s.config.API.Port)
	logrus.Infof("GoTell API started on: %s", l)
	return http.ListenAndServe(l, s.handler)
}

func NewServer(config *conf.Configuration, githubClient *github.Client) *Server {
	return NewServerWithVersion(config, githubClient, defaultVersion)
}

func NewServerWithVersion(config *conf.Configuration, githubClient *github.Client, version string) *Server {
	s := &Server{
		config:  config,
		client:  githubClient,
		version: version,
	}

	mux := kami.New()
	mux.LogHandler = logHandler
	mux.Use("/", timeRequest)
	mux.Use("/", jsonTypeRequired)
	mux.Get("/", s.index)
	mux.Post("/*path", s.postComment)

	corsHandler := cors.New(cors.Options{
		AllowedMethods:   []string{"GET", "POST", "PATCH", "PUT", "DELETE"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type"},
		ExposedHeaders:   []string{"Link", "X-Total-Count"},
		AllowCredentials: true,
	})

	s.handler = corsHandler.Handler(mux)
	return s
}

func timeRequest(ctx context.Context, w http.ResponseWriter, r *http.Request) context.Context {
	return context.WithValue(ctx, "_gotell_timing", time.Now())
}

func logHandler(ctx context.Context, wp mutil.WriterProxy, req *http.Request) {
	start := ctx.Value("_gotell_timing").(time.Time)
	logrus.WithFields(logrus.Fields{
		"method":   req.Method,
		"path":     req.URL.Path,
		"status":   wp.Status(),
		"duration": time.Since(start),
	}).Info("")
}

func jsonTypeRequired(ctx context.Context, w http.ResponseWriter, r *http.Request) context.Context {
	if r.Method == "POST" && r.Header.Get("Content-Type") != "application/json" {
		http.Error(w, "Content-Type must be application/json", 422)
		return nil
	}
	return ctx
}

func sendJSON(w http.ResponseWriter, status int, obj interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.Encode(obj)
}

func jsonError(w http.ResponseWriter, message string, status int) {
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.Encode(map[string]string{"msg": message})
}

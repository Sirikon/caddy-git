package git

import (
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/mholt/caddy/caddy/setup"
	"github.com/mholt/caddy/middleware"
)

const (
	// DefaultInterval is the minimum interval to delay before
	// requesting another git pull
	DefaultInterval time.Duration = time.Hour * 1
)

// Git configures a new Git service routine.
func Setup(c *setup.Controller) (middleware.Middleware, error) {
	git, err := parse(c)
	if err != nil {
		return nil, err
	}

	// repos configured with webhooks
	var hookRepos []*Repo

	// functions to execute at startup
	var startupFuncs []func() error

	// loop through all repos and and start monitoring
	for i := range git {
		repo := git.Repo(i)

		// If a HookUrl is set, we switch to event based pulling.
		// Install the url handler
		if repo.Hook.Url != "" {

			hookRepos = append(hookRepos, repo)

			startupFuncs = append(startupFuncs, func() error {
				return repo.Pull()
			})

		} else {
			startupFuncs = append(startupFuncs, func() error {

				// Start service routine in background
				Start(repo)

				// Do a pull right away to return error
				return repo.Pull()
			})
		}
	}

	// ensure the functions are executed once per server block
	// for cases like server1.com, server2.com { ... }
	c.OncePerServerBlock(func() error {
		c.Startup = append(c.Startup, startupFuncs...)
		return nil
	})

	// if there are repo(s) with webhook
	// return handler
	if len(hookRepos) > 0 {
		webhook := &WebHook{Repos: hookRepos}
		return func(next middleware.Handler) middleware.Handler {
			webhook.Next = next
			return webhook
		}, err
	}

	return nil, err
}

func parse(c *setup.Controller) (Git, error) {
	var git Git

	for c.Next() {
		repo := &Repo{Branch: "master", Interval: DefaultInterval, Path: c.Root}

		args := c.RemainingArgs()

		switch len(args) {
		case 2:
			repo.Path = filepath.Clean(c.Root + string(filepath.Separator) + args[1])
			fallthrough
		case 1:
			repo.URL = args[0]
		}

		for c.NextBlock() {
			switch c.Val() {
			case "repo":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				repo.URL = c.Val()
			case "path":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				repo.Path = filepath.Clean(c.Root + string(filepath.Separator) + c.Val())
			case "branch":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				repo.Branch = c.Val()
			case "key":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				repo.KeyPath = c.Val()
			case "interval":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				t, _ := strconv.Atoi(c.Val())
				if t > 0 {
					repo.Interval = time.Duration(t) * time.Second
				}
			case "hook":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				repo.Hook.Url = c.Val()

				// optional secret for validation
				if c.NextArg() {
					repo.Hook.Secret = c.Val()
				}
			case "hook_type":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				t := c.Val()
				if _, ok := handlers[t]; !ok {
					return nil, c.Errf("invalid hook type %v", t)
				}
				repo.Hook.Type = t
			case "then":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				command := c.Val()
				args := c.RemainingArgs()
				repo.Then = append(repo.Then, NewThen(command, args...))
			case "then_long":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				command := c.Val()
				args := c.RemainingArgs()
				repo.Then = append(repo.Then, NewLongThen(command, args...))
			default:
				return nil, c.ArgErr()
			}
		}

		// if repo is not specified, return error
		if repo.URL == "" {
			return nil, c.ArgErr()
		}

		// if private key is not specified, convert repository URL to https
		// to avoid ssh authentication
		// else validate git URL
		// Note: private key support not yet available on Windows
		var err error
		if repo.KeyPath == "" {
			repo.URL, repo.Host, err = sanitizeHTTP(repo.URL)
		} else {
			repo.URL, repo.Host, err = sanitizeGit(repo.URL)
			// TODO add Windows support for private repos
			if runtime.GOOS == "windows" {
				return nil, fmt.Errorf("private repository not yet supported on Windows")
			}
		}

		if err != nil {
			return nil, err
		}

		// validate git requirements
		if err = Init(); err != nil {
			return nil, err
		}

		// prepare repo for use
		if err = repo.Prepare(); err != nil {
			return nil, err
		}

		git = append(git, repo)

	}

	return git, nil
}

// sanitizeHTTP cleans up repository URL and converts to https format
// if currently in ssh format.
// Returns sanitized url, hostName (e.g. github.com, bitbucket.com)
// and possible error
func sanitizeHTTP(repoURL string) (string, string, error) {
	url, err := url.Parse(repoURL)
	if err != nil {
		return "", "", err
	}

	if url.Host == "" && strings.HasPrefix(url.Path, "git@") {
		url.Path = url.Path[len("git@"):]
		i := strings.Index(url.Path, ":")
		if i < 0 {
			return "", "", fmt.Errorf("invalid git url %s", repoURL)
		}
		url.Host = url.Path[:i]
		url.Path = "/" + url.Path[i+1:]
	}

	if url.User != nil {
		repoURL = "https://" + url.User.Username() + "@" + url.Host + url.Path
	} else {
		// Bitbucket require the user to be set into the HTTP URL
		if url.Host == "bitbucket.org" {
			segments := strings.Split(url.Path, "/")
			repoURL = "https://" + segments[1] + "@" + url.Host + url.Path
		} else {
			repoURL = "https://" + url.Host + url.Path
		}
	}

	// add .git suffix if missing
	if !strings.HasSuffix(repoURL, ".git") {
		repoURL += ".git"
	}

	return repoURL, url.Host, nil
}

// sanitizeGit cleans up repository url and converts to ssh format for private
// repositories if required.
// Returns sanitized url, hostName (e.g. github.com, bitbucket.com)
// and possible error
func sanitizeGit(repoURL string) (string, string, error) {
	repoURL = strings.TrimSpace(repoURL)

	// check if valid ssh format
	if !strings.HasPrefix(repoURL, "git@") || strings.Index(repoURL, ":") < len("git@a:") {
		// check if valid http format and convert to ssh
		if url, err := url.Parse(repoURL); err == nil && strings.HasPrefix(url.Scheme, "http") {
			repoURL = fmt.Sprintf("git@%v:%v", url.Host, url.Path[1:])
		} else {
			return "", "", fmt.Errorf("invalid git url %s", repoURL)
		}
	}
	hostURL := repoURL[len("git@"):]
	i := strings.Index(hostURL, ":")
	host := hostURL[:i]

	// add .git suffix if missing
	if !strings.HasSuffix(repoURL, ".git") {
		repoURL += ".git"
	}

	return repoURL, host, nil
}

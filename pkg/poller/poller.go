package poller

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jenkins-x/jx-helpers/v3/pkg/stringhelpers"

	"github.com/jenkins-x/jx-git-operator/pkg/constants"
	"github.com/jenkins-x/jx-git-operator/pkg/launcher"
	"github.com/jenkins-x/jx-git-operator/pkg/launcher/job"
	"github.com/jenkins-x/jx-git-operator/pkg/repo"
	"github.com/jenkins-x/jx-git-operator/pkg/repo/secret"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cmdrunner"
	"github.com/jenkins-x/jx-helpers/v3/pkg/files"
	"github.com/jenkins-x/jx-helpers/v3/pkg/gitclient"
	"github.com/jenkins-x/jx-helpers/v3/pkg/gitclient/cli"
	"github.com/jenkins-x/jx-helpers/v3/pkg/termcolor"
	"github.com/jenkins-x/jx-logging/v3/pkg/log"

	"k8s.io/client-go/kubernetes"
)

// Options the configuration options for the poller
type Options struct {
	GitClient  gitclient.Interface
	RepoClient repo.Interface
	Launcher   launcher.Interface

	// CommandRunner used to run git commands if no GitClient provided
	CommandRunner cmdrunner.CommandRunner

	// KubeClient is used to lazy create the repo client and launcher
	KubeClient kubernetes.Interface

	// Dir is the work directory. If not specified a temporary directory is created on startup.
	Dir string `env:"WORK_DIR"`

	// Namespace the namespace polled for `Secret` resources
	Namespace string `env:"NAMESPACE"`

	// GitBinary name of the git binary; defaults to `git`
	GitBinary string `env:"GIT_BINARY"`

	// PollDuration duration between polls
	PollDuration time.Duration `env:"POLL_DURATION"`

	// NoLoop disable the polling loop so that a single poll is performed only
	NoLoop bool `env:"NO_LOOP"`

	// NoResourceApply disable the applying of resources in a git repository at `.jx/git-operator/resources/*.yaml`
	NoResourceApply bool `env:"NO_RESOURCE_APPLY"`

	// Branch the branch to poll. If not specified defaults to the default branch from the clone
	Branch string `env:"BRANCH"`
}

// Run polls for git changes
func (o *Options) Run() error {
	err := o.ValidateOptions()
	if err != nil {
		return fmt.Errorf("invalid options: %w", err)
	}

	if o.Namespace != "" {
		log.Logger().Infof("looking in namespace %s for Secret resources with selector %s", o.Namespace, constants.DefaultSelector)
	}

	if !o.NoLoop {
		log.Logger().Infof("using poll duration %s", o.PollDuration.String())
	}
	for {
		err = o.Poll()
		if err != nil {
			return err
		}
		if o.NoLoop {
			return nil
		}
		time.Sleep(o.PollDuration)
	}
}

// Poll polls the available repositories
func (o *Options) Poll() error {
	err := o.ValidateOptions()
	if err != nil {
		return fmt.Errorf("invalid options: %w", err)
	}

	repos, err := o.RepoClient.List()
	if err != nil {
		return fmt.Errorf("failed to list repositories: %w", err)
	}

	if len(repos) == 0 {
		log.Logger().Infof("no repositories found")
		return nil
	}
	for _, r := range repos {
		err = o.pollRepository(r)
		if err != nil {
			return fmt.Errorf("failed to poll repository %s in namespace %s: %w", r.Name, r.Namespace, err)
		}
	}
	return nil
}

func (o *Options) pollRepository(r repo.Repository) error {
	name := r.Name
	safeGitURL := stringhelpers.SanitizeURL(r.GitURL)
	log.Logger().Infof("polling repository %s in namespace %s with git URL %s", name, r.Namespace, safeGitURL)

	dir := filepath.Join(o.Dir, name)
	exists, err := files.DirExists(dir)
	if err != nil {
		return fmt.Errorf("failed to check dir exists %s: %w", dir, err)
	}
	if !exists {
		log.Logger().Infof("cloning repository %s to %s", name, dir)
		_, err = o.GitClient.Command(o.Dir, "clone", r.GitURL, dir)
		if err != nil {
			return fmt.Errorf("failed to clone repository %s: %w", name, err)
		}
	} else {
		if o.Branch == "" {
			o.Branch, err = gitclient.Branch(o.GitClient, dir)
			if err != nil {
				//TODO: Evaluate if we should return instead of logging the error
				log.Logger().Warnf("failed to get the current branch %s\n", err.Error())
			}
			o.Branch = strings.TrimSpace(o.Branch)
			log.Logger().Infof("using main branch: %s", termcolor.ColorInfo(o.Branch))
		}
		_, err = o.GitClient.Command(dir, "pull", "origin", o.Branch)
		if err != nil {
			return fmt.Errorf("failed to pull repository %s: %w", name, err)
		}
	}
	text, err := o.GitClient.Command(dir, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("failed to find latest commit sha for repository %s: %w", name, err)
	}
	text = strings.TrimSpace(text)
	log.Logger().Infof("repository %s has latest commit sha %s", name, text)

	commitMessage, err := gitclient.GetLatestCommitMessage(o.GitClient, dir)
	if err != nil {
		return fmt.Errorf("failed to get the last commit message: %w", err)
	}
	commitAuthor, err := gitclient.GetLatestCommitAuthor(o.GitClient, dir)
	if err != nil {
		return fmt.Errorf("failed to get the last commit author: %w", err)
	}
	commitEmail, err := gitclient.GetLatestCommitAuthorEmail(o.GitClient, dir)
	if err != nil {
		return fmt.Errorf("failed to get the last commit author: %w", err)
	}
	commitDate, err := gitclient.GetLatestCommitDate(o.GitClient, dir)
	if err != nil {
		return fmt.Errorf("failed to get the last commit date: %w", err)
	}

	if text == "" {
		return fmt.Errorf("could not find latest commit sha for repository %s", name)
	}

	_, err = o.Launcher.Launch(&launcher.LaunchOptions{
		Repository:            r,
		GitSHA:                text,
		LastCommitAuthor:      commitAuthor,
		LastCommitAuthorEmail: commitEmail,
		LastCommitDate:        commitDate,
		LastCommitMessage:     commitMessage,
		Dir:                   dir,
		NoResourceApply:       o.NoResourceApply,
	})
	if err != nil {
		return fmt.Errorf("failed to launch job for %s: %w", name, err)
	}
	return nil
}

// ValidateOptions validates the options and lazily creates any resources required
func (o *Options) ValidateOptions() error {
	if o.CommandRunner == nil {
		o.CommandRunner = cmdrunner.QuietCommandRunner
	}
	if o.PollDuration.Milliseconds() == int64(0) {
		o.PollDuration = time.Second * 30
	}
	if o.GitClient == nil {
		o.GitClient = cli.NewCLIClient(o.GitBinary, o.CommandRunner)
	}
	var err error
	if o.RepoClient == nil {
		o.RepoClient, err = secret.NewClient(o.KubeClient, o.Namespace, constants.DefaultSelector)
		if err != nil {
			return fmt.Errorf("failed to create repo client: %w", err)
		}
	}
	if o.Launcher == nil {
		o.Launcher, err = job.NewLauncher(o.KubeClient, o.Namespace, constants.DefaultSelector, o.CommandRunner)
		if err != nil {
			return fmt.Errorf("failed to create launcher: %w", err)
		}
	}
	if o.Dir == "" {
		o.Dir, err = os.MkdirTemp("", "jx-git-operator-")
		if err != nil {
			return fmt.Errorf("failed to create temp dir: %w", err)
		}
	}
	return nil
}

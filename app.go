package main

import (
	"encoding/json"
	"errors"
	"fmt"
	gocli "github.com/gen64/go-cli"
	"github.com/gorilla/mux"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"reflect"
)

type App struct {
	cfg           Config
	githubPayload *GitHubPayload
	githubAPI     *GitHubAPI
	jenkinsAPI    *JenkinsAPI
	cli           *gocli.CLI
	cache         Cache
	wg            sync.WaitGroup
}

func (app *App) printIteration(i int, rc int) {
	log.Print("Retry: (" + strconv.Itoa(i+1) + "/" + strconv.Itoa(rc) + ")")
}

func (app *App) getCrumbAndSleep(u string, t string, rd int) (string, error) {
	crumb, err := app.jenkinsAPI.GetCrumb(app.cfg.Jenkins.BaseURL, u, t)
	if err != nil {
		log.Print("Error getting crumb")
		time.Sleep(time.Second * time.Duration(rd))
		return "", errors.New("Error getting crumb")
	}
	return crumb, nil
}

func (app *App) replacePathWithRepoAndNum(p string, r string, n int) string {
	s := strings.ReplaceAll(p, "{{.repository}}", r)
	s = strings.ReplaceAll(s, "{{.number}}", fmt.Sprintf("%d", n))
	return s
}

func (app *App) processJenkinsEndpointRetries(endpointDef *JenkinsEndpoint, repo string, num int, retryDelay int, retryCount int) error {
	iterations := int(0)
	if retryCount > 0 {
		for iterations < retryCount {
			app.printIteration(iterations, retryCount)

			crumb, err := app.getCrumbAndSleep(app.cfg.Jenkins.User, app.cfg.Jenkins.Token, retryDelay)
			if err != nil {
				iterations++
				continue
			}

			endpointPath := app.replacePathWithRepoAndNum(endpointDef.Path, repo, num)

			resp, err := app.jenkinsAPI.Post(app.cfg.Jenkins.BaseURL+"/"+endpointPath, app.cfg.Jenkins.User, app.cfg.Jenkins.Token, crumb)
			if err != nil {
				log.Print("Error from request to " + endpointPath)
				time.Sleep(time.Second * time.Duration(retryDelay))
				iterations++
				continue
			}

			log.Print("Posted to endpoint " + endpointPath)

			if !endpointDef.CheckHTTPStatus(resp.StatusCode) {
				rs := strconv.Itoa(resp.StatusCode)
				log.Print("HTTP Status " + rs + " different than expected ")
				time.Sleep(time.Second * time.Duration(retryDelay))
				iterations++
				continue
			}

			return nil
		}
	}
	return errors.New("Unable to post to endpoint " + endpointDef.Path)
}

func (app *App) triggerPRJob(repo string, num int) {
	log.Print(app.cfg)
	for _, endp := range app.cfg.Jenkins.Endpoints {
		rd, err := endp.GetRetryDelay()
		if err != nil {
			break
		}
		rc, err := endp.GetRetryCount()
		if err != nil {
			break
		}
		app.processJenkinsEndpointRetries(&endp, repo, num, rd, rc)
	}
}

func (app *App) updateCache(action string, repo string, num int, branch string, depsAfter []string, branchesOnly bool) {
	app.cache.mu.Lock()
	defer app.wg.Done()
	defer app.cache.mu.Unlock()

	// branches only
	if action == "opened" || action == "edited" || action == "reopened" {
		// set PR in Branches
		_, hasKey := app.cache.Branches[repo]
		if !hasKey {
			app.cache.Branches[repo] = map[int]string{}
		}
		app.cache.Branches[repo][num] = branch
	}

	if action == "closed" {
		// unset PR from Branches
		_, hasKey := app.cache.Branches[repo][num]
		if hasKey {
			delete(app.cache.Branches[repo], num)
		}
	}

	if branchesOnly {
		return
	}

	// dependencies and tidying up
	// TODO: this can be refactored as it became quite messy...
	depsBefore := map[string]int{}
	_, hasKey := app.cache.Dependencies[repo]
	if hasKey {
		_, hasKey2 := app.cache.Dependencies[repo][num]
		if hasKey2 {
			for r, n := range app.cache.Dependencies[repo][num] {
				depsBefore[r] = n
			}
		}
	}

	if action == "opened" || action == "edited" || action == "reopened" {
		_, hasKey = app.cache.Dependencies[repo]
		if !hasKey {
			app.cache.Dependencies[repo] = map[int]map[string]int{}
		}

		// dependencies are added in the 'tidy' loop
		app.cache.Dependencies[repo][num] = map[string]int{}

		// clean dependents as these are set in the 'tidy' loop
		for r, n := range depsBefore {
			_, hasKey := app.cache.Dependents[r][n][repo]
			if hasKey {
				delete(app.cache.Dependents[r][n], repo)
			}
			// tidy up: if dependency branch does not exist then tidy it up as well
			_, hasKey = app.cache.Branches[r][n]
			if !hasKey {
				// tidy up - remove entries for non-existing PR
				_, hasKey2 := app.cache.Dependencies[r][n]
				if hasKey2 {
					delete(app.cache.Dependencies[r], n)
				}
				_, hasKey2 = app.cache.Dependents[r][n]
				if hasKey2 {
					delete(app.cache.Dependents[r], n)
				}
			}
		}

		// add new dependencies
		for _, dep := range depsAfter {
			vals := strings.Split(dep, "#")
			i, err := strconv.Atoi(vals[1])
			if err == nil {
				_, hasKey := app.cache.Branches[vals[0]][i]
				if !hasKey {
					// tidy up - remove entries for non-existing PR
					_, hasKey2 := app.cache.Dependencies[vals[0]][i]
					if hasKey2 {
						delete(app.cache.Dependencies[vals[0]], i)
					}
					_, hasKey2 = app.cache.Dependents[vals[0]][i]
					if hasKey2 {
						delete(app.cache.Dependents[vals[0]], i)
					}
				} else {
					// set PR in Dependencies and Dependents
					if action == "opened" || action == "edited" || action == "reopened" {
						app.cache.Dependencies[repo][num][vals[0]] = i
	
						// set dependency PR in Dependents
						_, hasKey2 := app.cache.Dependents[vals[0]]
						if !hasKey2 {
							app.cache.Dependents[vals[0]] = map[int]map[string]int{}
						}
						_, hasKey2 = app.cache.Dependents[vals[0]][i]
						if !hasKey2 {
							app.cache.Dependents[vals[0]][i] = map[string]int{}
						}
						app.cache.Dependents[vals[0]][i][repo] = num
					}
				}
			}
		}
	}

	if action == "closed" {
		// unset PR in Dependencies
		_, hasKey = app.cache.Dependencies[repo][num]
		if hasKey {
			delete(app.cache.Dependencies[repo], num)
		}
		// unset PR in Dependents
		_, hasKey = app.cache.Dependents[repo][num]
		if hasKey {
			delete(app.cache.Dependents[repo], num)
		}
		// unset Dependent-PR connection if it exists
		for _, dep := range depsAfter {
			vals := strings.Split(dep, "#")
			i, err := strconv.Atoi(vals[1])
			if err == nil {
				_, hasKey1 := app.cache.Dependents[vals[0]][i][repo]
				if hasKey1 {
					if app.cache.Dependents[vals[0]][i][repo] == num {
						delete(app.cache.Dependents[vals[0]][i], repo)
					}
				}
				// additionally remove non-existing PRs as well
				_, hasKey := app.cache.Branches[vals[0]][i]
				if !hasKey {
					// tidy up - remove entries for non-existing PR
					_, hasKey2 := app.cache.Dependencies[vals[0]][i]
					if hasKey2 {
						delete(app.cache.Dependencies[vals[0]], i)
					}
					_, hasKey2 = app.cache.Dependents[vals[0]][i]
					if hasKey2 {
						delete(app.cache.Dependents[vals[0]], i)
					}
				}
			}
		}
	}

	if action == "edited" {
		if !reflect.DeepEqual(depsBefore, app.cache.Dependencies[repo][num]) {
			app.triggerPRJob(repo, num)
		}
	}
}

func (app *App) startHandler(cli *gocli.CLI) int {
	c, err := ioutil.ReadFile(cli.Flag("config"))
	if err != nil {
		log.Fatal("Error reading config file")
	}

	var cfg Config
	cfg.SetFromJSON(c)
	app.cfg = cfg

	repos, err := app.githubAPI.GetRepositoriesList(app.cfg.PullRequestDependsOn.Owner, app.cfg.PullRequestDependsOn.Organization, app.cfg.Token)
	if err != nil {
		log.Fatal("Error fetching repository list from GitHub")
	}

	filteredRepos := []string{}
	for _, repo := range repos {
		f := app.checkIfRepoShouldBeIncluded(repo)
		if f {
			filteredRepos = append(filteredRepos, repo)
		}
	}

	log.Print("The following repositories match rules in the config file:")
	log.Print(filteredRepos)

	// Nasty loop in a loop but this is executed just twice when app is initialized
	for _, repo := range filteredRepos {
		pullRequests, err := app.githubAPI.GetPullRequestList(app.cfg.PullRequestDependsOn.Owner, repo, app.cfg.Token)
		if err != nil {
			log.Fatal(fmt.Sprintf("Error fetching pull requests for %s", app.cfg.PullRequestDependsOn.Owner))
		}
		log.Print(fmt.Sprintf("The following pull requests have been found in the %s/%s repository", app.cfg.PullRequestDependsOn.Owner, repo))
		log.Print(pullRequests)

		for _, pr := range pullRequests {
			app.wg.Add(1)
			go app.updateCache("opened", pr.Repository, pr.Number, pr.Branch, pr.DependsOn, true)
			app.wg.Wait()
		}
	}

	// again same loop - sorry, dependencies have to be added once all PRs are available
	for _, repo := range filteredRepos {
		pullRequests, err := app.githubAPI.GetPullRequestList(app.cfg.PullRequestDependsOn.Owner, repo, app.cfg.Token)
		if err != nil {
			log.Fatal(fmt.Sprintf("Error fetching pull requests for %s", app.cfg.PullRequestDependsOn.Owner))
		}

		for _, pr := range pullRequests {
			app.wg.Add(1)
			go app.updateCache("opened", pr.Repository, pr.Number, pr.Branch, pr.DependsOn, false)
			app.wg.Wait()
		}
	}

	log.Print("The following Branches have been cached:")
	log.Print(app.cache.Branches)

	log.Print("The following Dependencies have been found:")
	log.Print(app.cache.Dependencies)

	done := make(chan bool)
	go app.startAPI()
	<-done
	return 0
}

func (app *App) startAPI() {
	router := mux.NewRouter()
	router.HandleFunc("/", app.apiHandler).Methods("POST", "GET")
	log.Print("Starting daemon listening on " + app.cfg.Port + "...")
	log.Fatal(http.ListenAndServe(":"+app.cfg.Port, router))
}

func (app *App) apiHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		app.apiHandlerPost(w, r)
	} else if r.Method == "GET" {
		app.apiHandlerGet(w, r)
	} else {
		w.WriteHeader(http.StatusBadRequest)
	}
}

func (app *App) apiHandlerGet(w http.ResponseWriter, r *http.Request) {
	if app.cfg.APITokenHeader != "" && app.cfg.APITokenValue != "" {
		if r.Header.Get(app.cfg.APITokenHeader) != app.cfg.APITokenValue {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}

	b, err := json.Marshal(app.cache)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "application/json")
	w.Write(b)
}

func (app *App) apiHandlerPost(w http.ResponseWriter, r *http.Request) {
	b, err := ioutil.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	event := app.githubPayload.GetEvent(r)
	signature := app.githubPayload.GetSignature(r)
	if app.cfg.Secret != "" {
		if !app.githubPayload.VerifySignature([]byte(app.cfg.Secret), signature, &b) {
			http.Error(w, "Signature verification failed", 401)
			return
			// log.Print("Signature verification failed - oh well")
		}
	}

	if event != "ping" {
		err = app.processGitHubPayload(&b, event)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
	w.Header().Set("content-type", "application/json")
}

func (app *App) processGitHubPayload(b *([]byte), event string) error {
	j := make(map[string]interface{})
	err := json.Unmarshal(*b, &j)
	if err != nil {
		return errors.New("Got non-JSON payload")
	}

	if app.cfg.PullRequestDependsOn != nil && event == "pull_request" {
		err = app.processPayloadOnPullRequestDependsOn(j, event)
		if err != nil {
			log.Print("Error processing github payload on PullRequestDependsOn. Breaking.")
		}
	}
	return nil
}

func (app *App) checkIfRepoShouldBeIncluded(repo string) bool {
	f := false
	for _, r := range *app.cfg.PullRequestDependsOn.Repositories {
		if !r.RegExp {
			if r.Name == "*" || r.Name == repo {
				f = true
				break
			}
		} else {
			m, _ := regexp.MatchString(r.Name, repo)
			if m {
				f = true
				break
			}
		}
	}
	for _, r := range *app.cfg.PullRequestDependsOn.ExcludeRepositories {
		if !r.RegExp {
			if r.Name == "*" || r.Name == repo {
				f = false
				break
			}
		} else {
			m, _ := regexp.MatchString(r.Name, repo)
			if m {
				f = false
				break
			}
		}
	}
	return f
}

func (app *App) processPayloadOnPullRequestDependsOn(j map[string]interface{}, event string) error {
	log.Print("Got payload")

	repo := app.githubPayload.GetRepository(j, event)
	// ref := app.githubPayload.GetRef(j, event)
	branch := app.githubPayload.GetBranch(j, event)
	action := app.githubPayload.GetAction(j, event)
	body := app.githubPayload.GetPullRequestBody(j)
	number := int(app.githubPayload.GetPullRequestNumber(j))

	log.Print(fmt.Sprintf("Got payload with action: %s", action))
	log.Print(fmt.Sprintf("Got payload with branch details: %s %d %s", repo, number, branch))

	if repo == "" {
		return nil
	}
	if body == "" {
		return nil
	}

	f := app.checkIfRepoShouldBeIncluded(repo)
	if !f {
		log.Print(fmt.Sprintf("Payload for %s %s %d %s got rejected due to not matching the rules", action, repo, number, branch))
		return nil
	}

	dependsOn := []string{}
	lines := strings.Split(body, "\r\n")
	for _, line := range lines {
		m, _ := regexp.MatchString("^DependsOn:[a-z0-9\\-_]{3,40}#[0-9]{1,10}$", line)
		if m {
			dependsOnLine := strings.Split(line, ":")
			dependsOn = append(dependsOn, dependsOnLine[1])
		}
	}
	log.Print("Got payload with the following DependsOn:")
	log.Print(dependsOn)

	app.wg.Add(1)
	go app.updateCache(action, repo, number, branch, dependsOn, false)
	app.wg.Wait()

	return nil
}

func (app *App) Run() {
	app.githubPayload = NewGitHubPayload()
	app.githubAPI = NewGitHubAPI()
	app.jenkinsAPI = NewJenkinsAPI()
	app.cache = Cache{
		Branches:     map[string]map[int]string{},
		Dependencies: map[string]map[int]map[string]int{},
		Dependents:   map[string]map[int]map[string]int{},
		Version:      "1",
	}

	os.Exit(app.cli.Run(os.Stdout, os.Stderr))
}

func (app *App) versionHandler(c *gocli.CLI) int {
	fmt.Fprintf(os.Stdout, VERSION+"\n")
	return 0
}

func NewApp() *App {
	app := &App{}

	app.cli = gocli.NewCLI("github-pullrequestd", "Tiny API to store GitHub Pull Request dependencies", "Nicholas Gasior <mg@gen64.io>")
	cmdStart := app.cli.AddCmd("start", "Starts API", app.startHandler)
	cmdStart.AddFlag("config", "c", "config", "Config file", gocli.TypePathFile|gocli.MustExist|gocli.Required, nil)
	_ = app.cli.AddCmd("version", "Prints version", app.versionHandler)

	return app
}

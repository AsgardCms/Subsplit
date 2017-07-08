package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/go-redis/redis"
	"github.com/pkg/errors"
	log "gopkg.in/inconshreveable/log15.v2"
)

var redisPreix string = ""

type config struct {
	WorkingDirectory string   `json:"working-directory"`
	URL              string   `json:"url"`
	Splits           []string `json:"splits"`
	SlackURL         string   `json:"slack_url"`
	Redis            struct {
		Host     string `json:"host"`
		Password string `json:"password"`
		DB       int    `json:"db"`
		Prefix   string `json:"prefix"`
	} `json:"redis"`
	HTTP struct {
		Port  int    `json:"port"`
		Route string `json:"route"`
	} `json:"http"`
}

type hook struct {
	Repository struct {
		URL string `json:"url"`
	} `json:"repository"`
	Ref string `json:"ref"`
}

type splitInstruction struct {
	head string
	tag  string
}

func main() {
	cfg, err := loadConfig(os.Args)
	if err != nil {
		log.Error(err.Error())
		os.Exit(1)
	}

	redisPreix = cfg.Redis.Prefix

	client := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Host,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})

	// Starts the webhook listener
	go startHTTPServer(client, cfg)

	// compile regex
	tagsRegex := regexp.MustCompile(`refs\/tags\/(.+)$`)
	headsRegex := regexp.MustCompile(`refs\/heads\/(.+)$`)

	for {
		log.Info("Waiting for a new push to split...")
		hookJSON, err := client.BRPopLPush(fmt.Sprintf("%s:incoming", redisPreix), fmt.Sprintf("%s:processing", redisPreix), 0).Result()
		if err != nil {
			skipHook(client, time.Now(), hookJSON, fmt.Sprintf("Error while waiting for a message : %s. Retrying...", err), err)
			continue
		}
		log.Info("Processing start")
		splitStart := time.Now()

		hook := &hook{}
		err = json.Unmarshal([]byte(hookJSON), hook)
		if err != nil {
			skipHook(client, splitStart, hookJSON, fmt.Sprintf("Error while unmarshalling the hook : %s.", hook), err)
			continue
		}

		if hook.Repository.URL != cfg.URL {
			skipHook(client, splitStart, hookJSON, fmt.Sprintf("The repository %s is not supported.", hook.Repository.URL), nil)
			continue
		}

		startMsg := ""
		splitInstruction := splitInstruction{}
		if tag := tagsRegex.FindStringSubmatch(hook.Ref); len(tag) == 2 {
			splitInstruction.tag = tag[1]
			startMsg = fmt.Sprintf("Started: splitting modules for tag %s", splitInstruction.tag)
		} else if head := headsRegex.FindStringSubmatch(hook.Ref); len(head) == 2 {
			splitInstruction.head = head[1]
			startMsg = fmt.Sprintf("Started: splitting modules for branch %s", splitInstruction.head)
		} else {
			skipHook(client, splitStart, hookJSON, fmt.Sprintf("Skipping request : unexpected reference detected: %s", hook.Ref), nil)
			continue
		}
		go sendSlack(cfg.SlackURL, startMsg)

		/*var wg sync.WaitGroup
		wg.Add(len(cfg.Splits))
		for i := range cfg.Splits {
			go func(i int) {
				defer wg.Done()
				split(cfg.WorkingDirectory, cfg.URL, cfg.Splits[i], splitInstruction)
			}(i)
		}
		wg.Wait()*/

		split(cfg.WorkingDirectory, cfg.URL, cfg.Splits, splitInstruction)

		client.LRem(fmt.Sprintf("%s:processing", redisPreix), 1, hookJSON)
		endMsg := fmt.Sprintf("Finished: splitting modules. It took %s", time.Now().Sub(splitStart).String())
		log.Info(endMsg)
		go sendSlack(cfg.SlackURL, endMsg)
	}
}

func split(workingDir string, repoUrl string, splits []string, si splitInstruction) {
	split := strings.Join(splits, " ")

	hash := fmt.Sprintf("%x", []byte(split))[:32]
	workingPath := path.Join(workingDir, hash)

	os.MkdirAll(workingPath, 0750)

	heads := fmt.Sprintf(`--heads="%s"`, si.head)
	tag := fmt.Sprintf(`--tags="%s"`, si.tag)
	if si.tag == "" {
		tag = "--no-tags"
	} else {
		heads = "--no-heads"
	}

	cmd := []string{
		fmt.Sprintf("cd %s", workingPath),
		fmt.Sprintf("(git subsplit init %s || true)", repoUrl),
		"git subsplit update",
		fmt.Sprintf(`git subsplit publish "%s" %s %s`, split, heads, tag),
		"cd ..",
		fmt.Sprintf("rm -rf %s", workingPath),
	}

	args := strings.Join(cmd, " && ")

	err := exec.Command("sh", "-c", args).Run()
	if err != nil {
		log.Error(fmt.Sprintf("Error while splitting %s : %s", split, err))
	}

}

func loadConfig(args []string) (*config, error) {
	cfgPath := ""
	if len(args) == 2 {
		cfgPath = args[1]
	} else {
		pwd, err := os.Getwd()
		if err != nil {
			return nil, errors.Wrap(err, "Unable to get pwd")
		}
		cfgPath = pwd + "/config.json"
	}

	log.Info(fmt.Sprintf("Looking for config in %s", cfgPath))
	configJSON, err := ioutil.ReadFile(cfgPath)
	if err != nil {
		return nil, errors.Wrapf(err, "Unable to open %s : %s", cfgPath, err)
	}

	cfg := &config{}
	err = json.Unmarshal(configJSON, cfg)
	if err != nil {
		return nil, errors.Wrapf(err, "Unable to load unmarshal config : %s", err)
	}

	return cfg, nil
}

type loggingFn func(msg string, ctx ...interface{})

func skipHook(client *redis.Client, start time.Time, hookJSON string, msg string, err error) {
	var loggingFn loggingFn
	if err != nil {
		loggingFn = log.Error
	} else {
		loggingFn = log.Info
	}

	loggingFn(msg, "start_time", start.String(), "end_time", time.Now().String())
	client.LRem(fmt.Sprintf("%s:processing", redisPreix), 1, hookJSON)
}

func sendSlack(url string, msg string) {
	if url == "" {
		return
	}

	var jsonStr = []byte(`{"channel": "#asgardcmscom", "username": "buildbot", "text": "` + msg + `", "icon_emoji": ":ghost:"}`)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonStr))
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
}

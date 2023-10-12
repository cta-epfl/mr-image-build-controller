package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/xanzy/go-gitlab"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type MrDeployStatus int64

const (
	NotDeployed MrDeployStatus = iota
	UpToDate
	UpdateAvailable
	Pending
	Desynchronized
)

const IMAGE_REGISTRY = "gitlab.cta-observatory.org:5555/bastien.wermeille/ctao-esap-sdc-portal/esap-mr"
const STAGING_BRANCH = "main"
const PRODUCTION_BRANCH = "main"

// TODO: Create new config struct -> including pid, target_branch and so on
type App struct {
	gitlab    *gitlab.Client
	pid       string
	k8sClient *kubernetes.Clientset

	mrCommits     map[string]bool
	prodCommits   map[string]bool
	stagingCommit map[string]bool
}

func NewApp(gitlabApi *gitlab.Client, pid string, k8sClient *kubernetes.Clientset) *App {
	return &App{
		gitlab:    gitlabApi,
		pid:       pid,
		k8sClient: k8sClient,

		mrCommits:     map[string]bool{},
		prodCommits:   map[string]bool{},
		stagingCommit: map[string]bool{},
	}
}

func (app *App) localBuild(context string, dockerfile string, imageName string, imageTag string) error {
	// TODO: launch kaniko with the right context, dockerfile and registryTag
	log.Printf("Image tag for kaniko : %s", imageName+":"+imageTag)
	cmd := exec.Command("/kaniko/executor",
		"-c", context,
		"-f", dockerfile,
		"-d", imageName+":"+imageTag)

	var outb, errb bytes.Buffer
	cmd.Stdout = &outb
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		fmt.Println("\nout:", outb.String(), "err:", errb.String())
	}
	return err
}

func (app *App) k8sBuild(ctx string, dockerfile string, imageName string, imageTag string, env string) error {
	// TODO: Spawn a new pod to build the image
	log.Printf("Image tag for kaniko : %s", imageName+":"+imageTag)

	podName := "img-build-" + env

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: "flux-system",
			Labels: map[string]string{
				"imag-build": "esap",
			},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:            podName,
					Image:           "gcr.io/kaniko-project/executor:v1.16.0",
					ImagePullPolicy: v1.PullIfNotPresent,
					Command: []string{
						"/kaniko/executor",
						"-c", ctx,
						"-f", dockerfile,
						"-d", imageName + ":" + imageTag,
					},
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      "registry-config",
							ReadOnly:  true,
							MountPath: "/kaniko/.docker/",
						},
					},
				},
			},
			Volumes: []v1.Volume{
				{
					Name: "registry-config",
					VolumeSource: v1.VolumeSource{
						Secret: &v1.SecretVolumeSource{
							SecretName: "esap-image-deploy-secret",
							Items: []v1.KeyToPath{
								{
									Key:  ".dockerconfigjson",
									Path: "config.json",
								},
							},
						},
					},
				},
			},
		},
	}

	createdPod, err := app.k8sClient.CoreV1().Pods(pod.Namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
	log.Printf("Pod created %s: %s", podName, createdPod)
	if err != nil {
		log.Printf("Error while creating k8s pod: %s", err)
	}
	return err
}

func (app *App) build(context string, dockerfile string, imageName string, imageTag string, env string) error {
	return app.k8sBuild(context, dockerfile, imageName, imageTag, env)
}

func (app *App) prepareContext(branch string, commit string) (string, error) {
	// clone env at provided commit
	token := os.Getenv("GITLAB_TOKEN")
	repository := os.Getenv("GITLAB_GIT")
	gitUrl := strings.Replace(repository, "https://", "git://oauth2:"+token+"@", 1)

	url := gitUrl + "#refs/heads/" + branch
	if commit != "" {
		fmt.Printf("Prepare context : %s", strings.Replace(repository, "https://", "git://oauth2:"+"TOKEN"+"@", 1)+"#refs/heads/"+branch+"#"+commit)
		url = url + "#" + commit
	} else {
		fmt.Printf("Prepare context : %s", strings.Replace(repository, "https://", "git://oauth2:"+"TOKEN"+"@", 1)+"#refs/heads/"+branch)
	}

	return url, nil
}

func (app *App) loopMr() {
	// TODO: Extract in option struct
	targetBranch := os.Getenv("TARGET_BRANCH")
	projectId := os.Getenv("GITLAB_PROJECT_ID")

	// TODO: Loop production -> tags
	// TODO: Loop staging -> most recent commit on a given branch
	// TODO: Loop MR -> most recent commit on a given MR

	openedState := "opened"
	openMergeRequests, _, err := app.gitlab.MergeRequests.ListProjectMergeRequests(projectId, &gitlab.ListProjectMergeRequestsOptions{
		TargetBranch: &targetBranch,
		State:        &openedState,
	})

	if err != nil {
		log.Printf("Unable to load MR")
		return
	}

	for _, mergeRequest := range openMergeRequests {
		commits, _, err := app.gitlab.MergeRequests.GetMergeRequestCommits(app.pid, mergeRequest.ID, &gitlab.GetMergeRequestCommitsOptions{PerPage: 1})
		// Latest commit
		if err != nil || len(commits) != 1 {
			log.Printf("No commit for MR %d", mergeRequest.ID)
			continue
		}
		latestCommit := commits[0]

		if _, ok := app.stagingCommit[latestCommit.ID]; !ok {
			// Prepare environment
			context, err := app.prepareContext(mergeRequest.SourceBranch, latestCommit.ID)
			if err != nil {
				log.Printf("Error while cloning MR environement: %s", err)
				continue
			}

			// Build image
			err = app.build(context, "esap/Dockerfile", IMAGE_REGISTRY, latestCommit.ID, "mr-"+latestCommit.ID)
			app.prodCommits[latestCommit.ID] = true

			if err != nil {
				log.Printf("Error while building MR image %d: %s", mergeRequest.ID, err)
			}
		}
	}
}

func (app *App) loopStaging() {
	// Load latest commit
	branche, _, err := app.gitlab.Branches.GetBranch(app.pid, STAGING_BRANCH)
	if err != nil {
		log.Printf("Unable to load branch: %s", STAGING_BRANCH)
		return
	}

	if _, ok := app.stagingCommit[branche.Commit.ID]; !ok {
		// Prepare environment
		context, err := app.prepareContext(STAGING_BRANCH, branche.Commit.ID)
		if err != nil {
			log.Printf("Error while cloning staging environement: %s", err)
			return
		}

		// Build image
		versionId := strconv.Itoa(int(branche.Commit.CommittedDate.Unix()))
		err = app.build(context, "esap/Dockerfile", IMAGE_REGISTRY, versionId, "staging-"+versionId)
		app.prodCommits[versionId] = true

		if err != nil {
			log.Printf("Error while building staging image '%s': %s", versionId, err)
		}
	}
}

func (app *App) loopProduction() {
	// Load latest tag
	tags, _, err := app.gitlab.Tags.ListTags(app.pid, &gitlab.ListTagsOptions{})
	if err != nil {
		return
	}

	var latestTag string = ""
	var latestTagCommit string = ""
	var latestParsedTag *semver.Version = nil

	for _, tag := range tags {
		// Check semver version and compare to latest tag
		t, err := semver.NewVersion(tag.Name)

		if err == nil && (latestParsedTag == nil || t.GreaterThan(latestParsedTag)) {
			latestTag = tag.Name
			latestParsedTag = t
			latestTagCommit = tag.Commit.ID
		}
	}

	if latestTag == "" {
		log.Printf("Unable to locate any valid production tag")
		return
	}

	if _, ok := app.prodCommits[latestTag]; !ok {
		// Prepare environment
		context, err := app.prepareContext(PRODUCTION_BRANCH, latestTagCommit)
		if err != nil {
			log.Printf("Error while cloning production environement: %s", err)
			return
		}

		// Build image
		err = app.build(context, "esap/Dockerfile", IMAGE_REGISTRY, latestTag, "prod-"+latestTag)
		app.prodCommits[latestTag] = true

		if err != nil {
			log.Printf("Error while building prod image '%s': %s", latestTagCommit, err)
		}
	}
}

func (app *App) loop() {
	// TODO: implement
	// 0. Local store of the latest built images per MR
	// 1. Load images from registry
	// 2. Load latest commit tag from repository
	// 3. Launch a build for the image
	// 4. Clean registry built images for MR closed
	// 5. Configure build for staging and prod

	app.loopProduction()
	app.loopStaging()
	app.loopMr()
}

func (app *App) Run() {
	ticker := time.NewTicker(2 * time.Minute)
	quit := make(chan bool)
	go func() {
		app.loop()
		for {
			log.Println("Loop start")
			select {
			case <-ticker.C:
				app.loop()
			case <-quit:
				ticker.Stop()
				return
			}
		}
	}()

	interruptChan := make(chan os.Signal, 1)
	signal.Notify(interruptChan, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	// Block until we receive our signal.
	<-interruptChan

	// create a deadline to wait for.
	_, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()
	quit <- true

	log.Println("Shutting down")
	os.Exit(0)
}

func k8sClient() *kubernetes.Clientset {
	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}
	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}
	return clientset
}

func main() {
	log.Println("Starting server")

	gitlabUrl := os.Getenv("GITLAB_URL")
	gitlabToken := os.Getenv("GITLAB_TOKEN")
	gitlabApi, err := gitlab.NewClient(gitlabToken, gitlab.WithBaseURL(gitlabUrl))
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}

	clientSet := k8sClient()

	app := NewApp(gitlabApi, os.Getenv("GITLAB_PROJECT_ID"), clientSet)
	app.Run()
}

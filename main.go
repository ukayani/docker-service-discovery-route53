package main

import (
	"context"
	"fmt"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	docker "github.com/docker/docker/client"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"io"
	"io/ioutil"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

func logAndFailIfError(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

const (
	containerStart  = "start"
	containerStop   = "die"
	serviceLabelKey = "svc.discovery.service.name"
	taskArnLabelKey = "com.amazonaws.ecs.task-arn"
)

type worker struct {
	id         int
	events     <-chan events.Message
	quit       <-chan bool
	failures   chan<- error
	processors map[string]processor
	wg         *sync.WaitGroup
}

type processor interface {
	process(events.Message) error
}

func createProcessor(fn func(message events.Message) error) processor {
	return &eventProcessor{
		processFn: fn,
	}
}

type eventProcessor struct {
	processFn func(message events.Message) error
}

func (p *eventProcessor) process(e events.Message) error {
	return p.processFn(e)
}

func newWorker(id int, events <-chan events.Message, quit <-chan bool, failures chan<- error,
	processors map[string]processor, wg *sync.WaitGroup) *worker {
	return &worker{
		id:         id,
		events:     events,
		quit:       quit,
		failures:   failures,
		processors: processors,
		wg:         wg,
	}
}

func (w *worker) Start() {
	go func() {
		defer w.wg.Done()
		for {
			select {
			case e := <-w.events:
				if p, ok := w.processors[e.Action]; ok {
					log.Infof("Worker: %v handled: %+v", w.id, e)
					if err := p.process(e); err != nil {
						w.failures <- err
					}
				} else {
					log.Infof("Ignoring event: %v", e.Action)
				}
			case value := <-w.quit:
				log.Infof("Worker: %v is quitting, with value %v", w.id, value)
				return
			}
		}
	}()
}

func mapErrorChannel(dockerErrors <-chan error, failures <-chan error) <-chan bool {

	quitChannel := make(chan bool)

	go func() {
		defer close(quitChannel)

		for {
			select {
			case e := <-dockerErrors:
				if e != io.EOF {
					log.Info("Received error on events channel")
					log.Error(e)
					quitChannel <- true
					return
				}
			case f := <-failures:
				log.Errorf("Error occurred during processing: %+v", f)
				quitChannel <- true
				return
			}
		}
	}()

	return quitChannel
}

func getContainerEvents(client *docker.Client, ctx context.Context) (<-chan events.Message, <-chan error) {
	eventFilter := filters.NewArgs()
	eventFilter.Add("Type", events.ContainerEventType)
	return client.Events(ctx, types.EventsOptions{Filters: eventFilter})
}

type predicate func(string) bool

func getLabelValue(container types.ContainerJSON, predicate predicate) (string, bool) {
	for k, v := range container.Config.Labels {
		if predicate(strings.ToLower(k)) {
			return v, true
		}
	}
	return "", false
}

var isServiceName predicate = func(k string) bool { return k == serviceLabelKey }
var isTaskArn predicate = func(k string) bool { return k == taskArnLabelKey }

var ipRegex = regexp.MustCompile(`"IPv4Addresses":\s*\[\s*"(.+)"\]`)

func getContainerIp(containerID string) (string, error) {
	// ifconfig docker0 | grep -oP 'inet addr:\K\S+'
	// run container with --add-host host:$(ifconfig docker0 | grep -oP 'inet addr:\K\S+')
	resp, err := http.Get(fmt.Sprintf("http://host:51678/v1/tasks?dockerid=%v", containerID))
	if err != nil {
		return "", err
	}

	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)

	if err != nil {
		return "", err
	}
	bodyStr := string(body)

	matches := ipRegex.FindStringSubmatch(bodyStr)
	if len(matches) == 0 {
		return "", errors.New("No IP address field found")
	}

	return matches[1], nil
}

func main() {

	nWorkers := 5
	wg := sync.WaitGroup{}

	ctx, cancel := context.WithCancel(context.Background())

	dockerClient, _ := docker.NewEnvClient()

	eventChannel, errorChannel := getContainerEvents(dockerClient, ctx)
	failureChannel := make(chan error, nWorkers)
	quitChannel := mapErrorChannel(errorChannel, failureChannel)

	startProcessor := createProcessor(func(e events.Message) error {
		log.Info("Processing container start")
		container, err := dockerClient.ContainerInspect(context.Background(), e.ID)

		if err != nil {
			return err
		}

		serviceName, ok := getLabelValue(container, isServiceName)

		if !ok {
			log.Info("Unable to find service name label. Skipping container")
			return nil
		}

		taskArn, ok := getLabelValue(container, isTaskArn)

		if !ok {
			log.Info("Unable to find task arn. Skipping container")
			return nil
		}

		log.Infof("Container for Service: %v with Task ARN: %v", serviceName, taskArn)

		ip, err := getContainerIp(container.ID)

		if err != nil {
			log.Error("Unable to find container IP")
			return err
		}

		log.Infof("Container IP: %v", ip)

		return nil
	})

	stopProcessor := createProcessor(func(e events.Message) error {
		log.Info("Processing container stop")
		container, err := dockerClient.ContainerInspect(context.Background(), e.ID)

		if err != nil {
			return err
		}

		serviceName, ok := getLabelValue(container, isServiceName)

		if !ok {
			log.Info("Unable to find service name label. Skipping container")
			return nil
		}

		taskArn, ok := getLabelValue(container, isTaskArn)

		if !ok {
			log.Info("Unable to find task arn. Skipping container")
			return nil
		}

		log.Infof("Container for Service: %v with Task ARN: %v", serviceName, taskArn)

		return nil
	})

	processors := map[string]processor{
		containerStart: startProcessor,
		containerStop:  stopProcessor,
	}

	for i := 0; i < nWorkers; i++ {
		wg.Add(1)
		log.Infof("Creating Worker: %v", i)
		worker := newWorker(i, eventChannel, quitChannel, failureChannel, processors, &wg)
		worker.Start()
	}

	wg.Wait()
	cancel()

	log.Info("All workers exited. Shutting down")
}

package main

import (
	"context"
	"errors"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	docker "github.com/docker/docker/client"
	log "github.com/sirupsen/logrus"
	"io"
	"sync"
)

func logAndFailIfError(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

const (
	containerStart = "start"
	containerStop  = "die"
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
	filters := filters.NewArgs()
	filters.Add("Type", events.ContainerEventType)
	return client.Events(ctx, types.EventsOptions{Filters: filters})
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
		log.Info("Container Start processor")
		log.Infof("Got event %+v", e)
		return nil
	})

	stopProcessor := createProcessor(func(e events.Message) error {
		log.Info("Container Stop processor")
		log.Infof("Got event %+v", e)
		return errors.New("failed stop processing")
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

package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/route53"
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
	serviceLabelKey = "discovery.service.name"
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

const defaultTTL = 0
const defaultWeight = 1

type hostedZone struct {
	id   string
	name string
}

func (h *hostedZone) withServiceName(serviceName string) string {
	return serviceName + "." + h.name
}

func createARecord(serviceName string, localIP string, hostedZone *hostedZone) error {
	sess, err := session.NewSession()
	if err != nil {
		return err
	}
	r53 := route53.New(sess)
	// This API call creates a new DNS record for this host
	dnsName := hostedZone.withServiceName(serviceName)
	params := &route53.ChangeResourceRecordSetsInput{
		ChangeBatch: &route53.ChangeBatch{
			Changes: []*route53.Change{
				{
					Action: aws.String(route53.ChangeActionUpsert),
					ResourceRecordSet: &route53.ResourceRecordSet{
						Name: aws.String(dnsName),
						// It creates an A record with the IP of the host running the agent
						Type: aws.String(route53.RRTypeA),
						ResourceRecords: []*route53.ResourceRecord{
							{
								Value: aws.String(localIP),
							},
						},
						SetIdentifier: aws.String(dnsName),
						// TTL=0 to avoid DNS caches
						TTL:    aws.Int64(defaultTTL),
						Weight: aws.Int64(defaultWeight),
					},
				},
			},
			Comment: aws.String("Host A Record Created"),
		},
		HostedZoneId: aws.String(hostedZone.id),
	}
	_, err = r53.ChangeResourceRecordSets(params)

	if err == nil {
		log.Info("Record " + dnsName + " created, resolves to " + localIP)
	}

	return err
}

func isMatch(rrs *route53.ResourceRecordSet, name string, ip string) bool {
	return rrs != nil &&
		rrs.Type != nil &&
		*rrs.Type == route53.RRTypeA &&
		*rrs.Name == name &&
		*rrs.ResourceRecords[0].Value == ip
}

func deleteARecord(serviceName string, localIP string, hostedZone *hostedZone) error {
	sess, err := session.NewSession()
	if err != nil {
		return err
	}
	r53 := route53.New(sess)
	// This API call creates a new DNS record for this host
	dnsName := hostedZone.withServiceName(serviceName)

	paramsList := &route53.ListResourceRecordSetsInput{
		HostedZoneId:    aws.String(hostedZone.id), // Required
		MaxItems:        aws.String("100"),
		StartRecordName: aws.String(dnsName),
		StartRecordType: aws.String(route53.RRTypeA),
	}
	more := true
	var recordSetToDelete *route53.ResourceRecordSet
	resp, err := r53.ListResourceRecordSets(paramsList)
	for more && recordSetToDelete == nil && err == nil {
		for _, rrset := range resp.ResourceRecordSets {
			if isMatch(rrset, dnsName, localIP) {
				recordSetToDelete = rrset
			}
		}

		more = resp.IsTruncated != nil && *resp.IsTruncated
		if more {
			paramsList.StartRecordIdentifier = resp.NextRecordIdentifier
			resp, err = r53.ListResourceRecordSets(paramsList)
		}
	}
	if err != nil {
		return err
	}
	if recordSetToDelete == nil {
		log.Error("Route53 record doesn't exist")
		return nil
	}

	// This API call deletes the DNS record for the service for this docker ID
	params := &route53.ChangeResourceRecordSetsInput{
		ChangeBatch: &route53.ChangeBatch{
			Comment: aws.String("Service Discovery Created Record"),
			Changes: []*route53.Change{
				{
					Action:            aws.String(route53.ChangeActionDelete),
					ResourceRecordSet: recordSetToDelete,
				},
			},
		},
		HostedZoneId: aws.String(hostedZone.id),
	}
	_, err = r53.ChangeResourceRecordSets(params)

	if err == nil {
		log.Info("Record " + dnsName + " deleted")
	}
	return err
}

func getHostedZone(id string) (*hostedZone, error) {
	sess, err := session.NewSession()
	if err != nil {
		return nil, err
	}
	r53 := route53.New(sess)

	params := &route53.GetHostedZoneInput{
		Id: aws.String(id),
	}
	hz, err := r53.GetHostedZone(params)

	if err != nil {
		return nil, err
	}

	return &hostedZone{
		id:   id,
		name: aws.StringValue(hz.HostedZone.Name),
	}, nil
}

func main() {

	var hostedZoneId = flag.String("hostedZoneId", "", "hosted zone ID in which to register records")

	flag.Parse()

	if len(*hostedZoneId) == 0 {
		logAndFailIfError(errors.New("hostedZoneId must be provided"))
	}

	hz, err := getHostedZone(*hostedZoneId)

	if err != nil {
		logAndFailIfError(err)
	}

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
		return createARecord(serviceName, ip, hz)
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

		ip, err := getContainerIp(container.ID)

		if err != nil {
			log.Error("Unable to find container IP")
			return err
		}

		return deleteARecord(serviceName, ip, hz)
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

# ECS Service Discovery via Route53

Enables basic service discovery for ECS services running tasks with the `awsvpc` network mode using Route53.

## How it works

The agent must run on every ECS Container Instance with a configured Route53 HostedZone.

Once running, the agent listens for container start and stop events and registers/de-registers their respective IPs
in the configured HostedZone.

Services for which you wish to enable service discovery should have a **container label** with the following key:

- `discovery.service.name`

The value for this label should be the name of the service.

For example, for the Hosted Zone  with name `.service.discovery.` and a container label of `simple-service`,
the agent will register `simple-service.service.discovery` with the container IP.


If there are multiple tasks in the service a weighted A Record will be created for each task with each task given the
same weight. In the example above, this would mean `simple-service.service.discovery` would have a set of IPs corresponding
to all running tasks associated with it.

## Running as a container

- The agent requires access to the docker daemon to listen to container events so we need to mount in the docker socket
- We also feed in a host entry to enable our container to talk to the host. We need this to be able to call the ecs agent endpoint to get container IP.
- Setting the container to restart always as it is designed to shut down when it encounters any failures

```bash
docker run --restart always -v /var/run/docker.sock:/var/run/docker.sock --add-host host:$(ifconfig docker0 | grep -oP 'inet addr:\K\S+') kayaniu/service-discovery-agent-route53:latest --hostedZoneId=[Hosted Zone Id Here] 
```

## Policies for Container Instances

Container instances need to have permission to complete some Route53 operations:

```
route53:GetHostedZone
route53:ListHostedZones
route53:ChangeResourceRecordSets
route53:ListResourceRecordSets
route53:ListHostedZonesByName

```

## TODOS

- Complete a sync operation upon launch of the agent, so it can process containers which are running before it launches (in case it restarts)
- Create an accompanying Lambda which periodically does cleanup in case a container instance dies


## AWS ECS Service Discovery

With the launch of AWS's own service discovery mechanism, this agent is really just a proof of concept.

Please use [AWS ECS Service Discovery](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/service-discovery.html)

## Inspiration

[AWS ECS Service Discovery DNS Github](https://github.com/awslabs/service-discovery-ecs-dns)

## Internals

- Using the docker client library to subscribe to docker container events
- Creating a fixed size worker pool to process docker events 
- Workers in this pool can signal failure which will result in the agent waiting for all workers to complete their in flight
work and then shut down
- Using the AWS Go SDK to complete route53 operations
- Calls internal ECS agent `http://localhost:51678/v1/tasks?dockerid={}` endpoint to retrieve IP of container
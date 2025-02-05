package connector

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/events"
	dockerclient "github.com/docker/docker/client"
	"github.com/shellhub-io/shellhub/pkg/agent"
	log "github.com/sirupsen/logrus"
)

const AgentPlatformConnector = "connector"

var _ Connector = new(DockerConnector)

// DockerConnector is a struct that represents a connector that uses Docker as the container runtime.
type DockerConnector struct {
	mu sync.Mutex
	// server is the ShellHub address of the server that the agent will connect to.
	server string
	// tenant is the tenant ID of the namespace that the agent belongs to.
	tenant string
	// cli is the Docker client.
	cli *dockerclient.Client
	// privateKeys is the path to the directory that contains the private keys for the containers.
	privateKeys string
	// cancels is a map that contains the cancel functions for each container.
	// This is used to stop the agent for a container, marking as done its context and closing the agent.
	cancels map[string]context.CancelFunc
}

// NewDockerConnector creates a new [Connector] that uses Docker as the container runtime.
func NewDockerConnector(server string, tenant string, privateKey string) (Connector, error) {
	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}

	return &DockerConnector{
		server:      server,
		tenant:      tenant,
		cli:         cli,
		privateKeys: privateKey,
		cancels:     make(map[string]context.CancelFunc),
	}, nil
}

// events returns the docker events.
func (d *DockerConnector) events(ctx context.Context) (<-chan events.Message, <-chan error) {
	return d.cli.Events(ctx, types.EventsOptions{})
}

func (d *DockerConnector) List(ctx context.Context) ([]string, error) {
	containers, err := d.cli.ContainerList(ctx, types.ContainerListOptions{})
	if err != nil {
		return nil, err
	}

	list := make([]string, len(containers))
	for i, container := range containers {
		list[i] = container.ID
	}

	return list, nil
}

// Start starts the agent for the container with the given ID.
func (d *DockerConnector) Start(ctx context.Context, id string) {
	id = id[:12]

	d.mu.Lock()
	ctx, d.cancels[id] = context.WithCancel(ctx)
	d.mu.Unlock()

	privateKey := fmt.Sprintf("%s/%s.key", d.privateKeys, id)
	go initContainerAgent(ctx, Container{
		ID:            id,
		ServerAddress: d.server,
		Tenant:        d.tenant,
		PrivateKey:    privateKey,
		Cancel:        d.cancels[id],
	})
}

// Stop stops the agent for the container with the given ID.
func (d *DockerConnector) Stop(_ context.Context, id string) {
	id = id[:12]

	d.mu.Lock()
	defer d.mu.Unlock()

	cancel, ok := d.cancels[id]
	if ok {
		cancel()
		delete(d.cancels, id)
	}
}

// Listen listens for events and starts or stops the agent for the containers.
func (d *DockerConnector) Listen(ctx context.Context) error {
	containers, err := d.List(ctx)
	if err != nil {
		return err
	}

	for _, container := range containers {
		d.Start(ctx, container)
	}

	events, errs := d.events(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errs:
			return err
		case container := <-events:
			// NOTICE: "start" and "die" Docker's events are call every time a new container start or stop,
			// independently how the command was run. For example, if a container was started with `docker run -d`, the
			// "start" event will be called, but if the same container was started with `docker start <container-id>`,
			// the "start" event will be called too. The same happens with the "die" event.
			switch container.Action {
			case "start":
				d.Start(ctx, container.ID)
			case "die":
				d.Stop(ctx, container.ID)
			}
		}
	}
}

// initContainerAgent initializes the agent for a container.
func initContainerAgent(ctx context.Context, container Container) {
	agent.AgentVersion = os.Getenv("SHELLHUB_VERSION")
	agent.AgentPlatform = AgentPlatformConnector

	cfg := &agent.Config{
		ServerAddress:     container.ServerAddress,
		TenantID:          container.Tenant,
		PrivateKey:        container.PrivateKey,
		PreferredHostname: container.ID,
		PreferredIdentity: container.ID,
		Mode:              agent.ModeConnector,
		KeepAliveInterval: 30,
	}

	log.WithFields(log.Fields{
		"id":             container.ID,
		"identity":       cfg.PreferredIdentity,
		"hostname":       cfg.PreferredHostname,
		"tenant_id":      cfg.TenantID,
		"server_address": cfg.ServerAddress,
		"timestamp":      time.Now(),
	}).Info("Connector container started")

	ag, err := agent.NewAgentWithConfig(cfg)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"id":            container.ID,
			"configuration": cfg,
		}).Fatal("Failed to create agent")
	}

	if err := ag.Initialize(); err != nil {
		log.WithError(err).WithFields(log.Fields{
			"id":            container.ID,
			"configuration": cfg,
		}).Fatal("Failed to initialize agent")
	}

	go func() {
		if err := ag.Ping(ctx, 0); err != nil {
			log.WithError(err).WithFields(log.Fields{
				"id":             container.ID,
				"identity":       cfg.PreferredIdentity,
				"hostname":       cfg.PreferredHostname,
				"tenant_id":      cfg.TenantID,
				"server_address": cfg.ServerAddress,
				"timestamp":      time.Now(),
			}).Fatal("Failed to ping server")
		}

		log.WithFields(log.Fields{
			"id":             container.ID,
			"identity":       cfg.PreferredIdentity,
			"hostname":       cfg.PreferredHostname,
			"tenant_id":      cfg.TenantID,
			"server_address": cfg.ServerAddress,
			"timestamp":      time.Now(),
		}).Info("Stopped pinging server")
	}()

	log.WithFields(log.Fields{
		"id":             container.ID,
		"identity":       cfg.PreferredIdentity,
		"hostname":       cfg.PreferredHostname,
		"tenant_id":      cfg.TenantID,
		"server_address": cfg.ServerAddress,
		"timestamp":      time.Now(),
	}).Info("Listening for connections")

	// NOTICE(r): listing for connection and wait for a channel message to close the agent. It will receives
	// this mensagem when something out of this goroutine send a `done`, what will cause the agent closes
	// and no more connection to be allowed until it be started again.
	if err := ag.Listen(ctx); err != nil {
		log.WithError(err).WithFields(log.Fields{
			"id":             container.ID,
			"identity":       cfg.PreferredIdentity,
			"hostname":       cfg.PreferredHostname,
			"tenant_id":      cfg.TenantID,
			"server_address": cfg.ServerAddress,
			"timestamp":      time.Now(),
		}).Fatal("Failed to listen for connections")
	}

	log.WithFields(log.Fields{
		"id":             container.ID,
		"identity":       cfg.PreferredIdentity,
		"hostname":       cfg.PreferredHostname,
		"tenant_id":      cfg.TenantID,
		"server_address": cfg.ServerAddress,
	}).Info("Connector container done")
}

package kubernetes

import (
	"context"
	"fmt"
	"io"
	"sync"

	"emperror.dev/errors"
	"github.com/apex/log"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/kubectyl/kuber/config"
	"github.com/kubectyl/kuber/environment"
	"github.com/kubectyl/kuber/events"
	"github.com/kubectyl/kuber/remote"
	"github.com/kubectyl/kuber/system"
)

type Metadata struct {
	Image string
	Stop  remote.ProcessStopConfiguration
}

// Ensure that the Docker environment is always implementing all the methods
// from the base environment interface.
var _ environment.ProcessEnvironment = (*Environment)(nil)

type Environment struct {
	mu sync.RWMutex

	// The public identifier for this environment. In this case it is the Docker container
	// name that will be used for all instances created under it.
	Id string

	// The environment configuration.
	Configuration *environment.Configuration

	meta *Metadata

	// The Docker client being used for this instance.
	config *rest.Config
	client *kubernetes.Clientset

	// Controls the hijacked response stream which exists only when we're attached to
	// the running container instance.
	stream remotecommand.Executor

	// Holds the stats stream used by the polling commands so that we can easily close it out.
	stats io.ReadCloser

	emitter *events.Bus

	logCallbackMx sync.Mutex
	logCallback   func([]byte)

	// Tracks the environment state.
	st *system.AtomicString
}

// New creates a new base Kubernetes environment. The ID passed through will be the
// ID that is used to reference the container from here on out. This should be
// unique per-server (we use the UUID by default). The container does not need
// to exist at this point.
func New(id string, m *Metadata, c *environment.Configuration) (*Environment, error) {
	config, cli, err := environment.Cluster()
	if err != nil {
		return nil, err
	}

	e := &Environment{
		Id:            id,
		Configuration: c,
		meta:          m,
		config:        config,
		client:        cli,
		st:            system.NewAtomicString(environment.ProcessOfflineState),
		emitter:       events.NewBus(),
	}

	return e, nil
}

func (e *Environment) GetServiceDetails() []v1.Service {
	list, err := e.client.CoreV1().Services(config.Get().Cluster.Namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: "uuid=" + e.Id,
	})
	if err != nil {
		return nil
	}

	return list.Items
}

func (e *Environment) log() *log.Entry {
	return log.WithField("environment", e.Type()).WithField("container_id", e.Id)
}

func (e *Environment) Type() string {
	return "docker"
}

// SetStream sets the current stream value from the Docker client. If a nil
// value is provided we assume that the stream is no longer operational and the
// instance is effectively offline.
func (e *Environment) SetStream(s remotecommand.Executor) {
	e.mu.Lock()
	e.stream = s
	e.mu.Unlock()
}

// IsAttached determines if this process is currently attached to the
// container instance by checking if the stream is nil or not.
func (e *Environment) IsAttached() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.stream != nil
}

// Events returns an event bus for the environment.
func (e *Environment) Events() *events.Bus {
	return e.emitter
}

// Exists determines if the container exists in this environment. The ID passed
// through should be the server UUID since containers are created utilizing the
// server UUID as the name and docker will work fine when using the container
// name as the lookup parameter in addition to the longer ID auto-assigned when
// the container is created.
func (e *Environment) Exists() (bool, error) {
	_, err := e.client.CoreV1().Pods(config.Get().Cluster.Namespace).Get(context.Background(), e.Id, metav1.GetOptions{})
	if err != nil {
		// If this error is because the container instance wasn't found via Docker we
		// can safely ignore the error and just return false.
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// IsRunning determines if the server's docker container is currently running.
// If there is no container present, an error will be raised (since this
// shouldn't be a case that ever happens under correctly developed
// circumstances).
//
// You can confirm if the instance wasn't found by using client.IsErrNotFound
// from the Docker API.
//
// @see docker/client/errors.go
func (e *Environment) IsRunning(ctx context.Context) (bool, error) {
	c, err := e.client.CoreV1().Pods(config.Get().Cluster.Namespace).Get(ctx, e.Id, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	if c.Status.Phase == v1.PodRunning {
		return true, nil
	}
	return false, nil
}

// ExitState returns the container exit state, the exit code and whether or not
// the container was killed by the OOM killer.
func (e *Environment) ExitState() (uint32, bool, error) {
	c, err := e.client.CoreV1().Pods(config.Get().Cluster.Namespace).Get(context.Background(), e.Id, metav1.GetOptions{})
	if err != nil {
		// I'm not entirely sure how this can happen to be honest. I tried deleting a
		// container _while_ a server was running and kuber gracefully saw the crash and
		// created a new container for it.
		//
		// However, someone reported an error in Discord about this scenario happening,
		// so I guess this should prevent it? They didn't tell me how they caused it though
		// so that's a mystery that will have to go unsolved.
		//
		// @see https://github.com/pterodactyl/panel/issues/2003
		if apierrors.IsNotFound(err) {
			return 1, false, nil
		}
		return 0, false, err
	}

	if len(c.Status.ContainerStatuses) != 0 {
		if c.Status.ContainerStatuses[0].State.Terminated != nil {
			// OOMKilled
			if c.Status.ContainerStatuses[0].State.Terminated.ExitCode == 137 {
				return 137, true, nil
			}

			return uint32(c.Status.ContainerStatuses[0].State.Terminated.ExitCode), false, nil
		}
	}
	return 1, false, nil
}

// Config returns the environment configuration allowing a process to make
// modifications of the environment on the fly.
func (e *Environment) Config() *environment.Configuration {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.Configuration
}

// SetStopConfiguration sets the stop configuration for the environment.
func (e *Environment) SetStopConfiguration(c remote.ProcessStopConfiguration) {
	e.mu.Lock()
	e.meta.Stop = c
	e.mu.Unlock()
}

func (e *Environment) SetImage(i string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.meta.Image = i
}

func (e *Environment) State() string {
	return e.st.Load()
}

// SetState sets the state of the environment. This emits an event that server's
// can hook into to take their own actions and track their own state based on
// the environment.
func (e *Environment) SetState(state string) {
	if state != environment.ProcessOfflineState &&
		state != environment.ProcessStartingState &&
		state != environment.ProcessRunningState &&
		state != environment.ProcessStoppingState {
		panic(errors.New(fmt.Sprintf("invalid server state received: %s", state)))
	}

	// Emit the event to any listeners that are currently registered.
	if e.State() != state {
		// If the state changed make sure we update the internal tracking to note that.
		e.st.Store(state)
		e.Events().Publish(environment.StateChangeEvent, state)
	}
}

func (e *Environment) SetLogCallback(f func([]byte)) {
	e.logCallbackMx.Lock()
	defer e.logCallbackMx.Unlock()

	e.logCallback = f
}

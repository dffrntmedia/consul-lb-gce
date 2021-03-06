package consul

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/dffrntmedia/consul-lb-gce/registry"

	"github.com/golang/glog"
	consul "github.com/hashicorp/consul/api"
)

const (
	consulWatchTimeout  = 30 * time.Second
	consulRetryInterval = 15 * time.Second
)

var (
	// ErrNoAddress when no Consul address has been specified
	ErrNoAddress = errors.New("No Consul address specified")
)

// consulRegistry is a registry for local caching and further watching of Consul data.
type consulRegistry struct {
	client *consul.Client
	sync.RWMutex
	watchedServices map[string]*consulService
	tagsToWatch     []string
}

// consulService contains data belonging to the same service.
type consulService struct {
	registry.Service
	lastIndex uint64
	removed   bool
	running   bool
	done      chan struct{}
	tag       string
}

// NewRegistry returns a Consul-backed service registry
func NewRegistry(config *registry.Config) (registry.Registry, error) {
	// validate arguments
	if len(config.Addresses) < 1 {
		return nil, ErrNoAddress
	}

	// connect to Consul
	clientConfig := consul.DefaultConfig()
	// select first address alone
	clientConfig.Address = config.Addresses[0]
	client, err := consul.NewClient(clientConfig)
	if err != nil {
		return nil, err
	}

	// prepare registry
	return &consulRegistry{
		client:          client,
		watchedServices: make(map[string]*consulService),
		tagsToWatch:     config.TagsToWatch,
	}, nil
}

func (cr *consulRegistry) Run(upstream chan<- *registry.ServiceUpdate, done <-chan struct{}) {
	defer close(upstream)
	// stop all service watchers
	defer cr.stop()

	// internal update channel
	update := make(chan *consulService, 16)
	go cr.watchServices(update, done)

	for {
		select {
		case <-done: // quit
			return
		case srv := <-update:
			// was it removed?
			if srv.removed {
				close(srv.done)

				// send clearing update upstream.
				upstream <- &registry.ServiceUpdate{
					ServiceName: srv.Name,
					UpdateType:  registry.DELETED,
					Tag:         srv.tag,
				}
				break
			}
			// it wasn't removed, so launch watcher for service
			// but only if it wasn't running in the first place
			if !srv.running {
				go cr.watchService(srv, upstream)
				srv.running = true
				upstream <- &registry.ServiceUpdate{
					ServiceName: srv.Name,
					UpdateType:  registry.NEW,
					Tag:         srv.tag,
				}
			}
		}
	}
}

func (cr *consulRegistry) stop() {
	// lock prevents Run from terminating while the watchers attempt
	// to send on their channels.
	cr.Lock()
	defer cr.Unlock()

	for _, srv := range cr.watchedServices {
		close(srv.done)
	}
}

// watchServices retrieves updates from Consul's services endpoint and sends
// potential updates to the update channel.
func (cr *consulRegistry) watchServices(update chan<- *consulService, done <-chan struct{}) {
	var lastIndex uint64
	for {
		// ask Consul about services
		catalog := cr.client.Catalog()
		services, meta, err := catalog.Services(&consul.QueryOptions{
			// is we have previously asked, then we should behave and wait for changes
			WaitIndex: lastIndex,
			WaitTime:  consulWatchTimeout,
		})
		if err != nil {
			glog.Errorf("Error refreshing service list: %s", err)
			// failure here is not catastrophic, so retry
			time.Sleep(consulRetryInterval)
			continue
		}
		// if the index equals the previous one, the watch timed out with no update.
		if meta.LastIndex == lastIndex {
			continue
		}
		lastIndex = meta.LastIndex

		cr.Lock()
		select {
		case <-done: // app is terminating, die
			cr.Unlock()
			return
		default:
			// continue
		}
		// check for services not yet cached locally.
		for k, v := range services {
			// ignore all but the ones with specified tags
			ignore := true

			var properTag string

			// iterate service tags
			for _, tag := range v {
				// iterate possible tags and compare
				for _, tagToWatch := range cr.tagsToWatch {
					if tag == tagToWatch {
						ignore = false
						properTag = tag
						// TODO add tag to watchedService
					}
				}
			}
			// have any of the tags to be watched been found?
			if ignore {
				continue
			}

			// is it a new service?
			service, ok := cr.watchedServices[k]
			if !ok { // yes
				service = new(consulService)
				service.Name = k
				service.done = make(chan struct{})
				service.tag = properTag
				cr.watchedServices[k] = service
				// since src.running == false, registry will start watching this service
				// before sending updates upstream
				update <- service
			}

		}
		// check for deleted services we should remove from cache
		for name, srv := range cr.watchedServices {
			if _, ok := services[name]; !ok {
				srv.removed = true
				// watchService will take care of sending this upstream
				update <- srv
				delete(cr.watchedServices, name)
			}
		}
		cr.Unlock()
	}
}

// watchService retrieves updates about a service from Consul's service endpoint.
// On a potential update, all service instances are pushed upstream.
func (cr *consulRegistry) watchService(service *consulService, upstream chan<- *registry.ServiceUpdate) {
	catalog := cr.client.Catalog()
	for {
		nodes, meta, err := catalog.Service(service.Name, "", &consul.QueryOptions{
			WaitIndex: service.lastIndex,
			WaitTime:  consulWatchTimeout,
		})
		if err != nil {
			glog.Errorf("Error refreshing service %s: %s", service.Name, err)
			time.Sleep(consulRetryInterval)
			continue
		}
		// If the index equals the previous one, the watch timed out with no update.
		if meta.LastIndex == service.lastIndex {
			continue
		}
		service.lastIndex = meta.LastIndex
		service.Instances = make(map[string]*registry.ServiceInstance, len(nodes))

		for _, node := range nodes {
			service.Instances[fmt.Sprintf("%s:%d", node.ServiceAddress, node.ServicePort)] = &registry.ServiceInstance{
				Host:    node.Node,
				Address: node.Address,
				Tags:    node.ServiceTags,
				Port:    strconv.Itoa(node.ServicePort),
			}
		}

		cr.Lock()
		select {
		case <-service.done:
			cr.Unlock()
			return
		default:
			// continue
		}

		// tell upstream about the updates
		upstream <- &registry.ServiceUpdate{
			ServiceName:      service.Name,
			UpdateType:       registry.CHANGED,
			ServiceInstances: service.Instances,
			Tag:              service.tag,
		}
		cr.Unlock()
	}
}

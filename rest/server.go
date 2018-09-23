package rest

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-phorce/dolly/metrics"
	"github.com/go-phorce/dolly/netutil"
	"github.com/go-phorce/dolly/rest/ready"
	"github.com/go-phorce/dolly/rest/tlsconfig"
	"github.com/go-phorce/dolly/tasks"
	"github.com/go-phorce/dolly/xhttp"
	"github.com/go-phorce/dolly/xhttp/context"
	"github.com/go-phorce/dolly/xhttp/httperror"
	"github.com/go-phorce/dolly/xhttp/marshal"
	"github.com/go-phorce/dolly/xlog"
	"github.com/juju/errors"
	"go.uber.org/dig"
)

var logger = xlog.NewPackageLogger("github.com/go-phorce/dolly", "rest")

// MaxRequestSize specifies max size of regular HTTP Post requests in bytes, 64 Mb
const MaxRequestSize = 64 * 1024 * 1024

const (
	// EvtSourceStatus specifies source for service Status
	EvtSourceStatus = "status"
	// EvtServiceStarted specifies Service Started event
	EvtServiceStarted = "service started"
	// EvtServiceStopped specifies Service Stopped event
	EvtServiceStopped = "service stopped"
)

// ClusterMember provides information about cluster member
type ClusterMember struct {
	// ID is the member ID for this member.
	ID string `protobuf:"bytes,1,opt,name=ID,proto3" json:"id,omitempty"`
	// Name is the human-readable name of the member. If the member is not started, the name will be an empty string.
	Name string `protobuf:"bytes,2,opt,name=name,proto3" json:"name,omitempty"`
	// PeerURLs is the list of URLs the member exposes to the cluster for communication.
	PeerURLs []string `protobuf:"bytes,3,rep,name=peers" json:"peers,omitempty"`
}

// ClusterInfo is an interface to provide basic info about the cluster
type ClusterInfo interface {
	// NodeID returns the ID of the node in the cluster
	NodeID() string

	NodeName() string

	// LeaderID returns the ID of the leader
	LeaderID() string

	// ClusterMembers returns the list of members in the cluster
	ClusterMembers() ([]*ClusterMember, error)
}

// Server is an interface to provide server status
type Server interface {
	ClusterInfo
	Name() string
	Version() string
	RoleName() string
	HostName() string
	LocalIP() string
	Port() string
	StartedAt() time.Time
	Uptime() time.Duration
	LocalCtx() context.Context
	Service(name string) Service
	// IsReady indicates that all subservices are ready to serve
	IsReady() bool

	// Call Event to record a new Auditable event
	// Audit event
	// source indicates the area that the event was triggered by
	// eventType indicates the specific event that occured
	// identity specifies the identity of the user that triggered this event, typically this is <role>/<cn>
	// contextID specifies the request ContextID that the event was triggered in [this can be used for cross service correlation of logs]
	// raftIndex indicates the index# of the raft log in RAFT that the event occured in [if applicable]
	// message contains any additional information about this event that is eventType specific
	Audit(source string,
		eventType string,
		identity string,
		contextID string,
		raftIndex uint64,
		message string)

	AddService(s Service)
	StartHTTP() error
	StopHTTP()

	Scheduler() tasks.Scheduler
}

// server is responsible for exposing the collection of the services
// as a single HTTP server
type server struct {
	Server
	container      *dig.Container
	context        context.Context
	auditor        Auditor
	authz          Authz
	cluster        ClusterInfo
	httpConfig     HTTPServerConfig
	tlsConfig      TLSInfoConfig
	tlsloader      *tlsconfig.KeypairReloader
	rolename       string
	hostname       string
	port           string
	ipaddr         string
	version        string
	startedAt      time.Time
	withClientAuth bool
	scheduler      tasks.Scheduler
	services       map[string]Service
	lock           sync.RWMutex
}

// New creates a new instance of the server
func New(
	rolename string,
	auditor Auditor,
	authz Authz,
	httpConfig HTTPServerConfig,
	tlsConfig TLSInfoConfig,
	cluster ClusterInfo,
	version string,
) (Server, error) {
	var err error
	ipaddr, err := netutil.GetLocalIP()
	if err != nil {
		ipaddr = "127.0.0.1"
		logger.Errorf("api=rest.New, reason=unable_determine_ipaddr, use='%s', err=[%v]", ipaddr, errors.ErrorStack(err))
	}

	return &server{
		auditor:    auditor,
		authz:      authz,
		context:    context.NewForRole(rolename),
		services:   map[string]Service{},
		scheduler:  tasks.NewScheduler(),
		cluster:    cluster,
		httpConfig: httpConfig,
		tlsConfig:  tlsConfig,
		rolename:   rolename,
		startedAt:  time.Now().UTC(),
		version:    version,
		hostname:   GetHostName(httpConfig.GetBindAddr()),
		port:       GetPort(httpConfig.GetBindAddr()),
		ipaddr:     ipaddr,
	}, nil
}

// LocalCtx specifies local context for the server
func (server *server) LocalCtx() context.Context {
	return server.context
}

// AddService provides a service registration for the server
func (server *server) AddService(s Service) {
	server.lock.Lock()
	defer server.lock.Unlock()
	server.services[s.Name()] = s
}

// Scheduler returns task scheduler for the server
func (server *server) Scheduler() tasks.Scheduler {
	return server.scheduler
}

// Service returns a registered server
func (server *server) Service(name string) Service {
	server.lock.Lock()
	defer server.lock.Unlock()
	return server.services[name]
}

// RoleName returns the name of the server role
func (server *server) RoleName() string {
	return server.rolename
}

// HostName returns the host name of the server
func (server *server) HostName() string {
	return server.hostname
}

// NodeName returns the node name in the cluster
func (server *server) NodeName() string {
	if server.cluster != nil {
		return server.cluster.NodeName()
	}
	return server.HostName()
}

// Port returns the port name of the server
func (server *server) Port() string {
	return server.port
}

// LocalIP returns the IP address of the server
func (server *server) LocalIP() string {
	return server.ipaddr
}

// StartedAt returns the time when the server started
func (server *server) StartedAt() time.Time {
	return server.startedAt
}

// Uptime returns the duration the server was up
func (server *server) Uptime() time.Duration {
	return time.Now().UTC().Sub(server.startedAt)
}

// Version returns the version of the server
func (server *server) Version() string {
	return server.version
}

// Name returns the server name
func (server *server) Name() string {
	return server.httpConfig.GetServiceName()
}

func (server *server) NodeID() string {
	if server.cluster == nil {
		return ""
	}
	return server.cluster.NodeID()
}

func (server *server) LeaderID() string {
	if server.cluster == nil {
		return ""
	}
	return server.cluster.LeaderID()
}

// IsLeader returns whether the server is the leader or not
func (server *server) ClusterMembers() ([]*ClusterMember, error) {
	if server.cluster == nil {
		return nil, errors.NotSupportedf("cluster")
	}
	return server.cluster.ClusterMembers()
}

// IsReady returns true when the server is ready to serve
func (server *server) IsReady() bool {
	for _, ss := range server.services {
		if !ss.IsReady() {
			return false
		}
	}
	return true
}

// Audit create an audit event
func (server *server) Audit(source string,
	eventType string,
	identity string,
	contextID string,
	raftIndex uint64,
	message string) {
	if server.auditor != nil {
		server.auditor.Audit(source, eventType, identity, contextID, raftIndex, message)
	} else {
		// {contextID}:{identity}:{raftIndex}:{source}:{type}:{message}
		logger.Infof("audit:%s:%s:%s:%s:%d:%s\n",
			source, eventType, identity, contextID, raftIndex, message)
	}
}

// StartHTTP will verify all the TLS related files are present and start the actual HTTPS listener for the server
func (server *server) StartHTTP() error {
	bindAddr := server.httpConfig.GetBindAddr()
	var err error

	// Main server
	if _, err = net.ResolveTCPAddr("tcp", bindAddr); err != nil {
		return errors.Annotatef(err, "api=StartHTTP, reason=ResolveTCPAddr, addr='%s'", bindAddr)
	}

	srv := &http.Server{
		IdleTimeout: time.Hour * 2,
		ErrorLog:    xlog.Stderr,
	}

	var httpsListener net.Listener

	if server.tlsConfig != nil && server.tlsConfig.GetKeyFile() != "" {
		withClientAuth := server.tlsConfig.GetClientCertAuth()
		certFile := server.tlsConfig.GetCertFile()
		keyFile := server.tlsConfig.GetKeyFile()
		server.withClientAuth = withClientAuth != nil && *withClientAuth
		tlsConfig, err := tlsconfig.BuildFromFiles(certFile, keyFile, server.tlsConfig.GetTrustedCAFile(), server.withClientAuth)
		if err != nil {
			return errors.Annotatef(err, "api=StartHTTP, reason=BuildFromFiles, cert='%s', key='%s'",
				certFile, keyFile)
		}

		// Start listening on main server over TLS
		httpsListener, err = tls.Listen("tcp", bindAddr, tlsConfig)
		if err != nil {
			return errors.Annotatef(err, "api=StartHTTP, reason=unable_listen, address='%s'", bindAddr)
		}

		srv.TLSConfig = tlsConfig

		server.tlsloader, err = tlsconfig.NewKeypairReloader(certFile, keyFile, 5*time.Second)
		if err != nil {
			return errors.Annotatef(err, "api=StartHTTP, reason=NewKeypairReloader, cert='%s', key='%s'",
				certFile, keyFile)
		}
		srv.TLSConfig.GetCertificate = server.tlsloader.GetKeypairFunc()

		go certExpirationPublisherTask(server)
		server.Scheduler().Add(tasks.NewTaskAtIntervals(1, tasks.Hours).Do("servertls", certExpirationPublisherTask, server))
	} else {
		srv.Addr = bindAddr
	}

	readyHandler := ready.NewServiceStatusVerifier(server, server.NewMux())
	metricsmux := xhttp.NewRequestMetrics(readyHandler)
	allowProfiling := server.httpConfig.GetAllowProfiling()
	if allowProfiling != nil && *allowProfiling {
		if metricsmux, err = xhttp.NewRequestProfiler(metricsmux, server.httpConfig.GetProfilerDir(), nil, xhttp.LogProfile()); err != nil {
			return err
		}
	}

	srv.Handler = metricsmux

	if httpsListener != nil {
		go func() {
			logger.Infof("api=StartHTTP, port=%v, status=starting, mode=TLS", bindAddr)
			if err := srv.Serve(httpsListener); err != nil {
				//panic, only if address is already in use, not for other errors like
				//Serve error while stopping the server, which is a valid error
				if netutil.IsAddrInUse(err) {
					logger.Panicf("api=StartHTTP, err=%v", errors.Trace(err))
				}
				logger.Errorf("api=StartHTTP, err=%v", errors.Trace(err))
			}
		}()
	} else {
		go func() {
			logger.Infof("api=StartHTTP, port=%v, status=starting, mode=HTTP", bindAddr)
			if err := srv.ListenAndServe(); err != nil {
				//panic, only if address is already in use, not for other errors like
				//Serve error while stopping the server, which is a valid error
				if netutil.IsAddrInUse(err) {
					logger.Panicf("api=StartHTTP, err=%v", errors.Trace(err))
				}
				logger.Errorf("api=StartHTTP, err=%v", errors.Trace(err))
			}
		}()
	}

	if server.httpConfig.GetHeartbeatSecs() > 0 {
		task := tasks.NewTaskAtIntervals(uint64(server.httpConfig.GetHeartbeatSecs()), tasks.Seconds).Do("server", uptimeTask, server)
		server.Scheduler().Add(task)
	}

	server.scheduler.Start()
	server.Audit(
		EvtSourceStatus,
		EvtServiceStarted,
		server.context.Identity().String(),
		server.context.RequestID(),
		0,
		fmt.Sprintf("node='%s', address='%s', ClientAuth=%t",
			server.NodeName(), strings.TrimPrefix(bindAddr, ":"), server.withClientAuth),
	)

	return nil
}

func uptimeTask(server *server) {
	metrics.PublishHeartbeat(server.httpConfig.GetServiceName(), server.Uptime())
}

func certExpirationPublisherTask(server *server) {
	certFile := server.tlsConfig.GetCertFile()
	keyFile := server.tlsConfig.GetKeyFile()

	logger.Tracef("api=certExpirationPublisherTask, server=%s, cert='%s', key='%s'", server.Name(),
		certFile, keyFile)

	server.tlsloader.Reload()
	pair := server.tlsloader.Keypair()
	if pair != nil {
		cert, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			errors.Annotatef(err, "api=certExpirationPublisherTask, reason=unable_parse_tls_cert, file='%s'", certFile)
		} else {
			metrics.PublishCertExpirationInDays(cert, "server")
		}
	} else {
		logger.Warningf("api=certExpirationPublisherTask, reason=Keypair, server=%s", server.Name())
	}
}

// StopHTTP will perform a graceful shutdown of the serivce by
//		1) signally to the Load Balancer to remove this instance from the pool
//				by changing to response to /availability
//		2) cause new responses to have their Connection closed when finished
//				to force clients to re-connect [hopefully to a different instance]
//		3) wait the minShutdownTime to ensure the LB has noticed the status change
//		4) wait for existing requests to finish processing
//		5) step 4 is capped by a overrall timeout where we'll give up waiting
//			 for the requests to complete and will exit.
//
// it is expected that you don't try and use the server instance again
// after this. [i.e. if you want to start it again, create another server instance]
func (server *server) StopHTTP() {
	if server.tlsloader != nil {
		server.tlsloader.Close()
		server.tlsloader = nil
	}

	// stop scheduled tasks
	server.scheduler.Stop()

	// close services
	for _, f := range server.services {
		logger.Tracef("api=StopHTTP, service='%s'", f.Name())
		f.Close()
	}

	ut := server.Uptime() / time.Second * time.Second
	server.Audit(
		EvtSourceStatus,
		EvtServiceStopped,
		server.context.Identity().String(),
		server.context.RequestID(),
		0,
		fmt.Sprintf("node=%s, uptime=%s", server.NodeName(), ut),
	)
}

// NewMux creates a new http handler for the http server, typically you only
// need to call this directly for tests.
func (server *server) NewMux() http.Handler {
	router := NewRouter(server.notFoundHandler)

	for _, f := range server.services {
		f.Register(router)
	}
	logger.Debugf("api=NewMux, service_count=%d", len(server.services))

	var err error
	httpHandler := router.Handler()

	logger.Infof("api=NewMux, service=%s, withClientAuth=%t", server.Name(), server.withClientAuth)

	if server.withClientAuth && server.authz != nil {
		// authz wrapper
		server.authz.SetRoleMapper(context.RoleFromRequest)
		httpHandler, err = server.authz.NewHandler(httpHandler)
		if err != nil {
			panic(errors.ErrorStack(err))
		}
		// TODO: only allow configured certs
		// httpHandler = authz.NewClientCertVerifier(httpHandler)
	}

	// logging wrapper
	httpHandler = xhttp.NewRequestLogger(httpHandler, server.rolename, serverExtraLogger, time.Millisecond, server.httpConfig.GetPackageLogger())

	// role/contextID wrapper
	ctxHandler := context.NewContextHandler(httpHandler)
	return ctxHandler
}

func (server *server) notFoundHandler(w http.ResponseWriter, r *http.Request) {
	marshal.WriteJSON(w, r, httperror.New(http.StatusNotFound, "Not found", "URL doesn't exist: %s", r.RequestURI))
}

func serverExtraLogger(resp *xhttp.ResponseCapture, req *http.Request) []string {
	return []string{context.CorrelationIDFromRequest(req)}
}
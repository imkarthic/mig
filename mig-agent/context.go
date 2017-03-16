// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// Contributor:
// - Julien Vehent jvehent@mozilla.com [:ulfr]
package main

import (
	"bufio"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/jvehent/service-go"
	"github.com/streadway/amqp"
	"mig.ninja/mig"
	"mig.ninja/mig/mig-agent/agentcontext"
	"mig.ninja/mig/modules"
)

// Context contains all configuration variables as well as handlers for
// logs and channels
// Context is intended as a single structure that can be passed around easily.
type Context struct {
	ACL   mig.ACL
	Agent struct {
		BinPath, RunDir, QueueLoc, Mode, UID string
		Respawn                              bool
		CheckIn                              bool

		// A lock must be obtained before reading these values.
		Hostname  string
		Env       mig.AgentEnv
		Tags      interface{}
		RefreshTS time.Time

		// Stores a copy of the last agent context generated by
		// agentcontext.NewAgentContext, used primarily to determine
		// if the context has changed when we do a refresh
		lastAgentContext agentcontext.AgentContext

		// This mutex is used to protect the Agent struct as some
		// elements in the struct could be routinely updated by
		// the agent context update go-routine. Care should be taken
		// to ensure a lock is obtained before reading or using
		// values flagged as volatile.
		sync.Mutex
	}
	Channels struct {
		// internal
		Terminate                           chan string
		Log                                 chan mig.Log
		NewCommand                          chan []byte
		RunAgentCommand, RunExternalCommand chan moduleOp
		Results                             chan mig.Command
	}
	MQ struct {
		// configuration
		Host, User, Pass string
		Port             int
		// internal
		UseTLS bool
		conn   *amqp.Connection
		Chan   *amqp.Channel
		Bind   struct {
			Queue, Key string
			Chan       <-chan amqp.Delivery
		}
	}
	OpID    float64       // ID of the current operation, used for tracking
	Sleeper time.Duration // timer used when the agent has to sleep for a while
	Socket  struct {
		Bind string
	}
	Logging mig.Logging
}

// Update volatile/dynamic fields in c.Agent using information stored in
// the AgentContext. A lock should be obtained on c.Agent before this
// function is called.
func (c *Context) updateVolatileFromAgentContext(actx agentcontext.AgentContext) bool {
	ts := time.Now()
	c.Agent.Hostname = actx.Hostname
	c.Agent.Env.OS = actx.OS
	c.Agent.Env.Arch = actx.Architecture
	c.Agent.Env.Ident = actx.OSIdent
	c.Agent.Env.Init = actx.Init
	c.Agent.Env.Addresses = actx.Addresses
	c.Agent.Env.PublicIP = actx.PublicIP
	c.Agent.Env.AWS.InstanceID = actx.AWS.InstanceID
	c.Agent.Env.AWS.LocalIPV4 = actx.AWS.LocalIPV4
	c.Agent.Env.AWS.AMIID = actx.AWS.AMIID
	c.Agent.Env.AWS.InstanceType = actx.AWS.InstanceType
	if c.Agent.lastAgentContext.IsZero() {
		c.Agent.lastAgentContext = actx
		c.Agent.RefreshTS = ts
		return false
	}
	// See if anything changed since the last update
	if actx.Differs(c.Agent.lastAgentContext) {
		c.Agent.lastAgentContext = actx
		c.Agent.RefreshTS = ts
		return true
	}
	return false
}

// Init prepare the AMQP connections to the broker and launches the
// goroutines that will process commands received by the MIG Scheduler
func Init(foreground, upgrade bool) (ctx Context, err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("initAgent() -> %v", e)
		}
		if ctx.Channels.Log != nil {
			ctx.Channels.Log <- mig.Log{Desc: "leaving initAgent()"}.Debug()
		}
	}()
	// Pick up a lock on Context Agent field as we will be updating or reading it here and in
	// various functions called from here such as daemonize().
	ctx.Agent.Lock()
	defer ctx.Agent.Unlock()

	ctx.Agent.Tags = TAGS

	ctx.Logging, err = mig.InitLogger(LOGGINGCONF, "mig-agent")
	if err != nil {
		panic(err)
	}
	// create the go channels
	ctx, err = initChannels(ctx)
	if err != nil {
		panic(err)
	}
	// Logging GoRoutine,
	go func() {
		for event := range ctx.Channels.Log {
			_, err := mig.ProcessLog(ctx.Logging, event)
			if err != nil {
				fmt.Println("Unable to process logs")
			}
		}
	}()
	ctx.Channels.Log <- mig.Log{Desc: "Logging routine initialized."}.Debug()

	// Gather new agent context information to use as the context for this
	// agent invocation
	hints := agentcontext.AgentContextHints{
		DiscoverPublicIP: DISCOVERPUBLICIP,
		DiscoverAWSMeta:  DISCOVERAWSMETA,
		APIUrl:           APIURL,
		Proxies:          PROXIES[:],
	}
	actx, err := agentcontext.NewAgentContext(ctx.Channels.Log, hints)
	if err != nil {
		panic(err)
	}

	// defines whether the agent should respawn itself or not
	// this value is overriden in the daemonize calls if the agent
	// is controlled by systemd, upstart or launchd
	ctx.Agent.Respawn = ISIMMORTAL

	// Do initial assignment of values which could change over the lifetime
	// of the agent process
	ctx.updateVolatileFromAgentContext(actx)

	// Add the list of available modules to the agent's environment struct
	// This list will not change while the process is running
	var mlist []string
	for key := range modules.Available {
		mlist = append(mlist, key)
	}
	ctx.Agent.Env.Modules = mlist

	// Set some other values obtained from the agent context which will not
	// change while the process is running.
	ctx.Agent.RunDir = actx.RunDir
	ctx.Agent.BinPath = actx.BinPath

	// get the agent ID
	ctx.Agent.UID = actx.UID

	// set the agent message queue location
	ctx.Agent.QueueLoc = actx.QueueLoc

	// daemonize if not in foreground mode
	if !foreground {
		// give one second for the caller to exit
		time.Sleep(time.Second)
		ctx, err = daemonize(ctx, upgrade)
		if err != nil {
			panic(err)
		}
	}

	ctx.Sleeper = HEARTBEATFREQ
	if err != nil {
		panic(err)
	}

	// parse the ACLs
	ctx, err = initACL(ctx)
	if err != nil {
		panic(err)
	}

	connected := false
	// connect to the message broker
	//
	// If any proxies have been configured, we try to use those first. If they fail, or
	// no proxies have been setup, just attempt a direct connection.
	for _, proxy := range PROXIES {
		ctx.Channels.Log <- mig.Log{Desc: fmt.Sprintf("Trying proxy %v for relay connection", proxy)}.Debug()
		ctx, err = initMQ(ctx, true, proxy)
		if err != nil {
			ctx.Channels.Log <- mig.Log{Desc: fmt.Sprintf("Failed to connect to relay using proxy %s: '%v'", proxy, err)}.Info()
			continue
		}
		connected = true
		goto mqdone
	}
	// Try and proxy that has been specified in the environment
	ctx.Channels.Log <- mig.Log{Desc: "Trying proxies from environment for relay connection"}.Debug()
	ctx, err = initMQ(ctx, true, "")
	if err == nil {
		connected = true
		goto mqdone
	} else {
		ctx.Channels.Log <- mig.Log{Desc: fmt.Sprintf("Failed to connect to relay using HTTP_PROXY: '%v'", err)}.Info()
	}
	// Fall back to a direct connection
	ctx.Channels.Log <- mig.Log{Desc: "Trying direct relay connection"}.Debug()
	ctx, err = initMQ(ctx, false, "")
	if err == nil {
		connected = true
	} else {
		ctx.Channels.Log <- mig.Log{Desc: fmt.Sprintf("Failed to connect to relay directly: '%v'", err)}.Info()
	}
mqdone:
	if !connected {
		panic("Failed to connect to the relay")
	}

	// catch interrupts
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		sig := <-c
		ctx.Channels.Terminate <- sig.String()
	}()

	// Set the agent stats socket bind address if set in configuration
	if SOCKET != "" {
		ctx.Socket.Bind = SOCKET
	}

	return
}

func initChannels(orig_ctx Context) (ctx Context, err error) {
	ctx = orig_ctx
	ctx.Channels.Terminate = make(chan string, 12)
	ctx.Channels.NewCommand = make(chan []byte, 7)
	ctx.Channels.RunAgentCommand = make(chan moduleOp, 5)
	ctx.Channels.RunExternalCommand = make(chan moduleOp, 5)
	ctx.Channels.Results = make(chan mig.Command, 5)
	ctx.Channels.Log = make(chan mig.Log, 97)
	ctx.Channels.Log <- mig.Log{Desc: "leaving initChannels()"}.Debug()
	return
}

// parse the permissions from the configuration into an ACL structure
func initACL(orig_ctx Context) (ctx Context, err error) {
	ctx = orig_ctx
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("initACL() -> %v", e)
		}
		ctx.Channels.Log <- mig.Log{Desc: "leaving initACL()"}.Debug()
	}()
	for _, jsonPermission := range AGENTACL {
		var parsedPermission mig.Permission
		err = json.Unmarshal([]byte(jsonPermission), &parsedPermission)
		if err != nil {
			panic(err)
		}
		for permName, _ := range parsedPermission {
			desc := fmt.Sprintf("Loading permission named '%s'", permName)
			ctx.Channels.Log <- mig.Log{Desc: desc}.Debug()
		}
		ctx.ACL = append(ctx.ACL, parsedPermission)
	}
	return
}

func initMQ(orig_ctx Context, try_proxy bool, proxy string) (ctx Context, err error) {
	ctx = orig_ctx
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("initMQ() -> %v", e)
		}
		ctx.Channels.Log <- mig.Log{Desc: "leaving initMQ()"}.Debug()
	}()

	//Define the AMQP binding
	ctx.MQ.Bind.Queue = fmt.Sprintf("mig.agt.%s", ctx.Agent.QueueLoc)
	ctx.MQ.Bind.Key = fmt.Sprintf("mig.agt.%s", ctx.Agent.QueueLoc)

	// parse the dial string and use TLS if using amqps
	amqp_uri, err := amqp.ParseURI(AMQPBROKER)
	if err != nil {
		panic(err)
	}
	ctx.Channels.Log <- mig.Log{Desc: fmt.Sprintf("AMQP: host=%s, port=%d, vhost=%s", amqp_uri.Host, amqp_uri.Port, amqp_uri.Vhost)}.Debug()
	if amqp_uri.Scheme == "amqps" {
		ctx.MQ.UseTLS = true
	}

	// create an AMQP configuration with specific timers
	var dialConfig amqp.Config
	dialConfig.Heartbeat = 2 * ctx.Sleeper
	if try_proxy {
		// if in try_proxy mode, the agent will try to connect to the relay using a CONNECT proxy
		// but because CONNECT is a HTTP method, not available in AMQP, we need to establish
		// that connection ourselves, and give it back to the amqp.DialConfig method
		if proxy == "" {
			// try to get the proxy from the environemnt (variable HTTP_PROXY)
			target := "http://" + amqp_uri.Host + ":" + fmt.Sprintf("%d", amqp_uri.Port)
			req, err := http.NewRequest("GET", target, nil)
			if err != nil {
				panic(err)
			}
			proxy_url, err := http.ProxyFromEnvironment(req)
			if err != nil {
				panic(err)
			}
			if proxy_url == nil {
				panic("Failed to find a suitable proxy in environment")
			}
			proxy = proxy_url.Host
			ctx.Channels.Log <- mig.Log{Desc: fmt.Sprintf("Found proxy at %s", proxy)}.Debug()
		}
		ctx.Channels.Log <- mig.Log{Desc: fmt.Sprintf("Connecting via proxy %s", proxy)}.Debug()
		dialConfig.Dial = func(network, addr string) (conn net.Conn, err error) {
			// connect to the proxy
			conn, err = net.DialTimeout("tcp", proxy, 5*time.Second)
			if err != nil {
				return
			}
			// write a CONNECT request in the tcp connection
			fmt.Fprintf(conn, "CONNECT "+addr+" HTTP/1.1\r\nHost: "+addr+"\r\n\r\n")
			// verify status is 200, and flush the buffer
			status, err := bufio.NewReader(conn).ReadString('\n')
			if err != nil {
				return
			}
			if status == "" || len(status) < 12 {
				err = fmt.Errorf("Invalid status received from proxy: '%s'", status[0:len(status)-2])
				return
			}
			// 9th character in response should be "2"
			// HTTP/1.0 200 Connection established
			//          ^
			if status[9] != '2' {
				err = fmt.Errorf("Invalid status received from proxy: '%s'", status[0:len(status)-2])
				return
			}
			ctx.Agent.Env.IsProxied = true
			ctx.Agent.Env.Proxy = proxy
			return
		}
	} else {
		dialConfig.Dial = func(network, addr string) (net.Conn, error) {
			return net.DialTimeout(network, addr, 5*time.Second)
		}
	}

	if ctx.MQ.UseTLS {
		ctx.Channels.Log <- mig.Log{Desc: "Loading AMQPS TLS parameters"}.Debug()
		// import the client certificates
		cert, err := tls.X509KeyPair(AGENTCERT, AGENTKEY)
		if err != nil {
			panic(err)
		}

		// import the ca cert
		ca := x509.NewCertPool()
		if ok := ca.AppendCertsFromPEM(CACERT); !ok {
			panic("failed to import CA Certificate")
		}
		TLSconfig := tls.Config{Certificates: []tls.Certificate{cert},
			RootCAs:            ca,
			InsecureSkipVerify: false,
			Rand:               rand.Reader}

		dialConfig.TLSClientConfig = &TLSconfig
	}
	// Open AMQP connection
	ctx.Channels.Log <- mig.Log{Desc: "Establishing connection to relay"}.Debug()
	ctx.MQ.conn, err = amqp.DialConfig(AMQPBROKER, dialConfig)
	if err != nil {
		ctx.Channels.Log <- mig.Log{Desc: "Connection failed"}.Debug()
		panic(err)
	}

	ctx.MQ.Chan, err = ctx.MQ.conn.Channel()
	if err != nil {
		panic(err)
	}

	// Limit the number of message the channel will receive at once
	err = ctx.MQ.Chan.Qos(1, // prefetch count (in # of msg)
		0,     // prefetch size (in bytes)
		false) // is global

	_, err = ctx.MQ.Chan.QueueDeclare(ctx.MQ.Bind.Queue, // Queue name
		true,  // is durable
		false, // is autoDelete
		false, // is exclusive
		false, // is noWait
		nil)   // AMQP args
	if err != nil {
		panic(err)
	}

	err = ctx.MQ.Chan.QueueBind(ctx.MQ.Bind.Queue, // Queue name
		ctx.MQ.Bind.Key,    // Routing key name
		mig.Mq_Ex_ToAgents, // Exchange name
		false,              // is noWait
		nil)                // AMQP args
	if err != nil {
		panic(err)
	}

	// Consume AMQP message into channel
	ctx.MQ.Bind.Chan, err = ctx.MQ.Chan.Consume(ctx.MQ.Bind.Queue, // queue name
		"",    // some tag
		false, // is autoAck
		false, // is exclusive
		false, // is noLocal
		false, // is noWait
		nil)   // AMQP args
	if err != nil {
		panic(err)
	}

	return
}

func Destroy(ctx Context) (err error) {
	close(ctx.Channels.Terminate)
	close(ctx.Channels.NewCommand)
	close(ctx.Channels.RunAgentCommand)
	close(ctx.Channels.RunExternalCommand)
	close(ctx.Channels.Results)
	// give one second for the goroutines to close
	time.Sleep(1 * time.Second)
	ctx.MQ.conn.Close()
	return
}

// serviceDeploy stops, removes, installs and start the mig-agent service in one go
func serviceDeploy(orig_ctx Context) (ctx Context, err error) {
	ctx = orig_ctx
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("serviceDeploy() -> %v", e)
		}
		ctx.Channels.Log <- mig.Log{Desc: "leaving serviceDeploy()"}.Debug()
	}()
	svc, err := service.NewService("mig-agent", "MIG Agent", "Mozilla InvestiGator Agent")
	if err != nil {
		panic(err)
	}

	// TODO: FIX THIS. it appears that stopping a service on upstart will kill both the agent
	// running as a service, and the agent currently upgrading which isn't yet running as a service.

	// if already running, stop it. don't panic on error
	err = svc.Stop()
	if err != nil {
		ctx.Channels.Log <- mig.Log{Desc: fmt.Sprintf("Failed to stop service mig-agent: '%v'", err)}.Info()
	} else {
		ctx.Channels.Log <- mig.Log{Desc: "Stopped running mig-agent service"}.Info()
	}

	err = svc.Remove()
	if err != nil {
		// fail but continue, the service may not exist yet
		ctx.Channels.Log <- mig.Log{Desc: fmt.Sprintf("Failed to remove service mig-agent: '%v'", err)}.Info()
	} else {
		ctx.Channels.Log <- mig.Log{Desc: "Removed existing mig-agent service"}.Info()
	}
	err = svc.Install()
	if err != nil {
		panic(err)
	}
	ctx.Channels.Log <- mig.Log{Desc: "Installed mig-agent service"}.Info()
	err = svc.Start()
	if err != nil {
		panic(err)
	}
	ctx.Channels.Log <- mig.Log{Desc: "Started mig-agent service"}.Info()
	return
}

func refreshAgentEnvironment(ctx *Context) {
	for {
		time.Sleep(REFRESHENV)
		ctx.Channels.Log <- mig.Log{Desc: "refreshing agent environment"}.Info()
		ctx.Agent.Lock()
		hints := agentcontext.AgentContextHints{
			DiscoverPublicIP: DISCOVERPUBLICIP,
			DiscoverAWSMeta:  DISCOVERAWSMETA,
			APIUrl:           APIURL,
			Proxies:          PROXIES[:],
		}
		actx, err := agentcontext.NewAgentContext(ctx.Channels.Log, hints)
		if err != nil {
			ctx.Channels.Log <- mig.Log{Desc: fmt.Sprintf("error obtaining new agent context: %v", err)}.Err()
		} else {
			if ctx.updateVolatileFromAgentContext(actx) {
				ctx.Channels.Log <- mig.Log{Desc: "agent environment has changed"}.Info()
			}
		}
		ctx.Agent.Unlock()
	}
}

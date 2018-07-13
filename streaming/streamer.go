/* Copyright (c) 2016-2017 Gregor Riepl
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package streaming

import (
	"errors"
	"fmt"
	"github.com/onitake/restreamer/api"
	"github.com/onitake/restreamer/mpegts"
	"github.com/onitake/restreamer/util"
	"github.com/onitake/restreamer/event"
	"net/http"
	"sync"
	"time"
)

const (
	moduleStreamer = "streamer"
	//
	eventStreamerError        = "error"
	eventStreamerQueueStart   = "queuestart"
	eventStreamerStart        = "start"
	eventStreamerStop         = "stop"
	eventStreamerClientAdd    = "add"
	eventStreamerClientRemove = "remove"
	eventStreamerStreaming    = "streaming"
	eventStreamerClosed       = "closed"
	eventStreamerInhibit      = "inhibit"
	eventStreamerAllow        = "allow"
	//
	errorStreamerInvalidCommand = "invalidcmd"
	errorStreamerPoolFull       = "poolfull"
	errorStreamerOffline        = "offline"
)

var (
	// ErrAlreadyRunning is thrown when trying to connect a stream that is already online.
	ErrAlreadyRunning = errors.New("restreamer: service is already active")
	// ErrNotRunning is thrown trying to shut down a stopped stream.
	ErrNotRunning = errors.New("restreamer: service is not running")
	// ErrOffline is thrown when receiving a connection while the stream is offline
	ErrOffline = errors.New("restreamer: refusing connection on an offline stream")
	// ErrSlowRead is logged (not thrown) when a client can not handle the bandwidth.
	ErrSlowRead = errors.New("restreamer: send buffer overrun, increase client bandwidth")
	// ErrPoolFull is logged when the connection pool is full.
	ErrPoolFull = errors.New("restreamer: maximum number of active connections exceeded")
)

// Command is one of several possible constants.
// See StreamerCommandAdd for more information.
type Command int

const (
	// streamerCommandIgnore is a default dummy command
	streamerCommandIgnore Command = iota
	// streamerCommandStart is an internal start command, used to signal request
	// processing to commence.
	streamerCommandStart
	// StreamerCommandAdd signals a stream to add a connection.
	StreamerCommandAdd
	// StreamerCommandRemove signals a stream to remove a connection.
	StreamerCommandRemove
	// StreamerCommandInhibit signals that all connections should be closed
	// and not further connections should be allowed
	StreamerCommandInhibit
	// StreamerCommandAllow signals that new connections should be allowed
	StreamerCommandAllow
)

// ConnectionRequest encapsulates a request that new connection be added or removed.
type ConnectionRequest struct {
	// Command is the command to execute
	Command Command
	// Address is the remote client address
	Address string
	// Connection is the connection to add (if this is an Add command)
	Connection *Connection
	// Waiter is a WaitGroup that can be used to track handling of the connection
	// in the streaming thread. If it is non-nil, the streamer will signal
	// Done once the request has been handled.
	Waiter *sync.WaitGroup
	// Ok tells the caller if a connection was handled without error.
	// You should always wait on the Waiter before checking it.
	Ok bool
}

// Streamer implements a TS packet multiplier,
// distributing received packets on the input queue to the output queues.
// It also handles and manages HTTP connections when added to an HTTP server.
type Streamer struct {
	// lock is the outgoing connection pool lock
	lock sync.Mutex
	// broker is a global connection broker
	broker ConnectionBroker
	// queueSize defines the maximum number of packets to queue per outgoing connection
	queueSize int
	// running reflects the state of the stream: if true, the Stream thread is running and
	// incoming connections are allowed.
	// If false, incoming connections are blocked.
	running util.AtomicBool
	// stats is the statistics collector for this stream
	stats api.Collector
	// logger is a json logger
	logger *util.ModuleLogger
	// request is an unbuffered queue for requests to add or remove a connection
	request chan *ConnectionRequest
	// events is an event receiver
	events event.Notifiable
}

// ConnectionBroker represents a policy handler for new connections.
// It is used to determine if new connections can be accepted,
// based on arbitrary rules.
type ConnectionBroker interface {
	// Accept will be called on each incoming connection,
	// with the remote client address and the streamer that wants to accept the connection.
	Accept(remoteaddr string, streamer *Streamer) bool
	// Release will be called each time a client disconnects.
	// The streamer argument corresponds to a streamer that has previously called Accept().
	Release(streamer *Streamer)
}

// NewStreamer creates a new packet streamer.
// queue is an input packet queue.
// qsize is the length of each connection's queue (in packets).
// broker handles policy enforcement
// stats is a statistics collector object.
func NewStreamer(qsize uint, broker ConnectionBroker) *Streamer {
	logger := &util.ModuleLogger{
		Logger: &util.ConsoleLogger{},
		Defaults: util.Dict{
			"module": moduleStreamer,
		},
		AddTimestamp: true,
	}
	streamer := &Streamer{
		broker:    broker,
		queueSize: int(qsize),
		running:   util.AtomicFalse,
		stats:     &api.DummyCollector{},
		logger:    logger,
		request:   make(chan *ConnectionRequest),
	}
	// start the command eater
	go streamer.eatCommands()
	return streamer
}

// SetLogger assigns a logger
func (streamer *Streamer) SetLogger(logger util.JsonLogger) {
	streamer.logger.Logger = logger
}

// SetCollector assigns a stats collector
func (streamer *Streamer) SetCollector(stats api.Collector) {
	streamer.stats = stats
}

// SetNotifier assigns an event notifier
func (streamer *Streamer) SetNotifier(events event.Notifiable) {
	streamer.events = events
}

func (streamer *Streamer) SetInhibit(inhibit bool) {
	if inhibit {
		streamer.request<- &ConnectionRequest{
			Command: StreamerCommandInhibit,
		}
	} else {
		streamer.request<- &ConnectionRequest{
			Command: StreamerCommandAllow,
		}
	}
}

// eatCommands is started in the background to drain the command
// queue and wait for a start command, in which case it will exit.
func (streamer *Streamer) eatCommands() {
	running := true
	for running {
		select {
		case request := <-streamer.request:
			switch request.Command {
			case streamerCommandStart:
				streamer.logger.Log(util.Dict{
					"event":   eventStreamerQueueStart,
					"message": "Stopping eater process and starting real processing",
				})
				running = false
			default:
				// Eating all other commands
			}
			// make sure the caller isn't waiting forever
			if request.Waiter != nil {
				request.Waiter.Done()
			}
		}
	}
}

// Stream is the main stream multiplier loop.
// It reads data from the input queue and distributes it to the connections.
//
// This routine will block; you should run it asynchronously like this:
//
// queue := make(chan mpegts.Packet, inputQueueSize)
// go func() {
//   log.Fatal(streamer.Stream(queue))
// }
//
// or simply:
//
// go streamer.Stream(queue)
func (streamer *Streamer) Stream(queue <-chan mpegts.Packet) error {
	// interlock and check for availability first
	if !util.CompareAndSwapBool(&streamer.running, false, true) {
		return ErrAlreadyRunning
	}

	// create the local outgoing connection pool
	pool := make(map[*Connection]bool)
	// prevent new connections if this is true
	inhibit := false

	// stop the eater process
	streamer.request <- &ConnectionRequest{
		Command: streamerCommandStart,
	}

	streamer.logger.Log(util.Dict{
		"event":   eventStreamerStart,
		"message": "Starting streaming",
	})

	// loop until the input channel is closed
	running := true
	for running {
		select {
		case packet, ok := <-queue:
			if ok {
				// got a packet, distribute
				//log.Printf("Got packet (length %d):\n%s\n", len(packet), hex.Dump(packet))
				//log.Printf("Got packet (length %d)\n", len(packet))

				for conn, _ := range pool {
					select {
					case conn.Queue <- packet:
						// packet distributed, done
						//log.Printf("Queued packet (length %d):\n%s\n", len(packet), hex.Dump(packet))

						// report the packet
						streamer.stats.PacketSent()
					default:
						// queue is full
						//log.Print(ErrSlowRead)

						// report the drop
						streamer.stats.PacketDropped()
					}
				}
			} else {
				// channel closed, exit
				running = false
				// and stop everything
				util.StoreBool(&streamer.running, false)
			}
		case request := <-streamer.request:
			switch request.Command {
			case StreamerCommandRemove:
				streamer.logger.Log(util.Dict{
					"event":   eventStreamerClientRemove,
					"message": fmt.Sprintf("Removing client %s from pool", request.Address),
				})
				if !request.Connection.Closed {
					close(request.Connection.Queue)
				}
				delete(pool, request.Connection)
			case StreamerCommandAdd:
				// check if the connection can be accepted
				if !inhibit && streamer.broker.Accept(request.Address, streamer) {
					streamer.logger.Log(util.Dict{
						"event":   eventStreamerClientAdd,
						"remote":  request.Address,
						"message": fmt.Sprintf("Adding client %s to pool", request.Address),
					})
					pool[request.Connection] = true
					request.Ok = true
				} else {
					streamer.logger.Log(util.Dict{
						"event":   eventStreamerError,
						"error":   errorStreamerPoolFull,
						"remote":  request.Address,
						"message": fmt.Sprintf("Refusing connection from %s, pool is full or offline", request.Address),
					})
					request.Ok = false
				}
			case StreamerCommandInhibit:
				streamer.logger.Log(util.Dict{
					"event":   eventStreamerInhibit,
					"message": fmt.Sprintf("Turning stream offline"),
				})
				inhibit = true
				// close all downstream connections
				for conn, _ := range pool {
					close(conn.Queue)
				}
				// TODO implement inhibit in the check api
			case StreamerCommandAllow:
				streamer.logger.Log(util.Dict{
					"event":   eventStreamerAllow,
					"message": fmt.Sprintf("Turning stream online"),
				})
				inhibit = false
				// TODO implement inhibit in the check api
			default:
				streamer.logger.Log(util.Dict{
					"event":   eventStreamerError,
					"error":   errorStreamerInvalidCommand,
					"message": "Ignoring invalid command in started state",
				})
			}
			// signal the caller that we have handled the message
			if request.Waiter != nil {
				request.Waiter.Done()
			}
		}
	}

	// clean up
	for _ = range queue {
		// drain any leftovers
	}
	for conn, _ := range pool {
		close(conn.Queue)
	}

	// start the command eater again
	go streamer.eatCommands()

	streamer.logger.Log(util.Dict{
		"event":   eventStreamerStop,
		"message": "Ending streaming",
	})
	return nil
}

// ServeHTTP handles an incoming HTTP connection.
// Satisfies the http.Handler interface, so it can be used in an HTTP server.
func (streamer *Streamer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	// create the connection object first
	conn := NewConnection(writer, streamer.queueSize, request.RemoteAddr)
	conn.SetLogger(streamer.logger.Logger)
	// and pass it on
	command := &ConnectionRequest{
		Command:    StreamerCommandAdd,
		Address:    request.RemoteAddr,
		Connection: conn,
		Waiter:     &sync.WaitGroup{},
	}
	command.Waiter.Add(1)
	streamer.request <- command

	// wait for the handler
	command.Waiter.Wait()

	// verify that the connection was added
	if !command.Ok {
		// nope, destroy the connection
		conn = nil
		streamer.logger.Log(util.Dict{
			"event":   eventStreamerError,
			"error":   errorStreamerOffline,
			"message": fmt.Sprintf("Refusing connection from %s, stream is offline", request.RemoteAddr),
		})
	}

	if conn != nil {
		// connection will be handled, report
		streamer.stats.ConnectionAdded()
		// also notify the event queue
		streamer.events.NotifyConnect(1)

		streamer.logger.Log(util.Dict{
			"event":   eventStreamerStreaming,
			"message": fmt.Sprintf("Streaming to %s", request.RemoteAddr),
			"remote":  request.RemoteAddr,
		})

		start := time.Now()
		conn.Serve()
		duration := time.Since(start)

		// done, remove the stale connection
		streamer.request <- &ConnectionRequest{
			Command:    StreamerCommandRemove,
			Address:    request.RemoteAddr,
			Connection: conn,
		}
		// and drain the queue AFTER we have sent the shutdown signal
		for _ = range conn.Queue {
			// drain any leftovers
		}
		streamer.logger.Log(util.Dict{
			"event":    eventStreamerClosed,
			"message":  fmt.Sprintf("Connection from %s closed", request.RemoteAddr),
			"remote":   request.RemoteAddr,
			"duration": duration,
		})

		// and report
		streamer.events.NotifyConnect(-1)
		streamer.stats.ConnectionRemoved()
		streamer.stats.StreamDuration(duration)

		// also notify the broker
		streamer.broker.Release(streamer)
	} else {
		// Return a suitable error
		// TODO This should be 503 or 504, but client support seems to be poor
		// and the standards mandate nothing. Bummer.
		ServeStreamError(writer, http.StatusNotFound)
	}
}

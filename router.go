package main

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"os"
	"time"
	r "torgo/regagent"
)

const regServerIP = "cse461.cs.washington.edu"
const regServerPort = "46101"

const (
	create       uint8 = 1
	created      uint8 = 2
	relay        uint8 = 3
	destroy      uint8 = 4
	open         uint8 = 5
	opened       uint8 = 6
	openFailed   uint8 = 7
	createFailed uint8 = 8
)

type circuit struct {
	circuitID uint16
	agentID   uint32
}

// Maps from router ID to a sending channel.
var currentConnections = make(map[uint32](chan []byte))

var firstHopCircuit circuit

// Bijective mapping from (circuitID, agentID) to (circuitID, agentID).
// Mapping from (circuitID, agentID) to (0, 0) means it is the
var routingTable = make(map[circuit]circuit)
var routerID uint32

func StartRouter(routerName string, agentID uint32, port uint16) {
	routerID = agentID
	agent := new(r.Agent)
	agent.StartAgent(regServerIP, regServerPort, false)
	agent.Register("127.0.0.1", port, agentID, uint8(len(routerName)), routerName)
	go localServer(port)
	rand.Seed(time.Now().Unix())
	responses := agent.Fetch("Tor61Router")
	var circuitNodes []r.FetchResponse
	for i := 0; i < 3; i++ {
		circuitNodes = append(circuitNodes, responses[rand.Intn(len(responses))])
	}
	fmt.Println(circuitNodes)

}

// TODO:
// HandleConnection handles all data between two routers.
// Besides dealing with open and create,
// it needs to just multiplex the data to a channel representing
// the circuit ID.
//
// Whoever calls create circuit C needs to add to the global map
// C -> channel.
// Then, when this method receives a "created" message, it just puts
// it on that channel by performing a lookup (if not found, sends a
// create failed).
// Similarly for all relay messages.
//
// Suppose we are agent 3.
// Let's say this method gets a create message (circuit C from agent 2).
// It adds to the global map the circuit ID -> channel and sends
// a created reply. We spawn a thread to check the channel.
// Then, let's say we get a relay message. We put it on the channel
// and another thread, which continuously checks the channel for that circuit
// sees it's a relay extend message from circuit C (extend to agent 4).
// It checks if there's an existing connection to agent 4.
// If so, it puts on the write buffer a create message with circuit C'
// (unique between agent 3 and 4) and adds a map entry C' -> buffer.
// This message gets forwarded to agent 4 and agent 4 sends back a created
// message. The thread above from circuit C blocks on the C' buffer waiting
// for a response. It sees a created response so it adds an entry
// to the routing table (2, C) -> (4, C'). Then this thread
// looks up the writebuffer for agent 2 and writes relay extended.
func handleConnection(c net.Conn) {
	// New connection. First, wait for an "open".
	defer c.Close()
	cell := make([]byte, 512)
	_, err := c.Read(cell)
	if err != nil {
		return
	}
	toSend := make(chan []byte, 100)
	toRead := make(chan []byte, 100)
	otherAgentID := 0
	// Maps from circuit ID to a message type.
	// For example, if we just sent a create message on
	// circuit ID 6, then 6 would map to type create
	// since we just sent that message and are waiting
	// on a reply.
	circuitToLastType := make(map[uint16]uint8)
	circuitID, cellType := cellCIDAndType(cell)
	relayExtending := false
	if cellType == open {
		otherAgentID, agentOpened := readOpenCell(cell)
		if agentOpened != routerID {
			// Open cell sent with wrong router ID.
			cell[2] = openFailed
			c.Write(cell)
			return
		} else {
			// Opened connection.
			cell[2] = opened
			currentConnections[agentOpener] = toSend
			c.Write(cell)
		}
	} else {
		// Connection initiated but they did not send an open cell.
		return
	}

	// Thread that blocks on data to send and sends it.
	go func() {
		for {
			cell := <-toSend
			circuitID, cellType := cellCIDAndType(cell)
			circuitToLastType[circuitID] = cellType
			c.Write(cell)
		}
	}()
	// Thread that blocks on the socket waiting for data and puts
	// it in channel.
	go func() {
		for {
			buffer := make([]byte, 512)
			_, err := c.Read(buffer)
			if err != nil {
				continue
			}
			toRead <- buffer
		}
	}()
	for {
		// Wait for data to come on the channel then process it.
		cell := <-toRead
		circuitID, cellType := cellCIDAndType(cell)
		switch cellType {
		case create:
			// Other agent wants to create a new circuit.
			// Need to ensure that this circuit ID is unique.
			// We are now the endpoint of some circuit.
			_, ok := routingTable[circuit{circuitID, otherAgentID}]
			if ok {
				// Circuit already existed.
				cell[2] = createFailed
			} else {
				routingTable[circuit{circuitID, otherAgentID}] = circuit{0, 0}
				cell[2] = created
			}
			toSend <- cell
			continue
		case created:
			// This means we must have sent a create at some point.
			// Just to make sure:
			lastType := circuitToLastType[circuitID]
			if lastType != create {
				fmt.Println("fail?")
				continue
			}
			if !relayExtending {
				// If it's not extending a relay, then we must have
				// just sent the first create in a circuit.
				// This means that we created the first hop.
				firstHopCircuit = circuit{circuitID, otherAgentID}
			} else {
				// If it is extending a relay, we want to map the two
				// circuits to each other.

			}
			//			case relay
		}

	}
}

func localServer(port uint16) {
	l, err := net.Listen("tcp", "0.0.0.0:"+string(port))
	if err != nil {
		os.Exit(2)
	}
	defer l.Close()

	for {
		c, err := l.Accept()
		if err != nil {
			continue
		}
		handleConnection(c)
	}
}

func cellCIDAndType(cell []byte) (uint16, uint8) {
	return binary.BigEndian.Uint16(cell[:2]), cell[3]
}

func createCell(circuitID uint16, cellType uint8) []byte {
	header := make([]byte, 512)
	binary.BigEndian.PutUint16(header[:2], circuitID)
	header[2] = cellType
	return header
}

func createOpenCell(header []byte, agentOpener uint32, agentOpened uint32) []byte {
	binary.BigEndian.PutUint32(header[:3], agentOpener)
	binary.BigEndian.PutUint32(header[:7], agentOpened)
	return header
}

func readOpenCell(cell []byte) (uint32, uint32) {
	agentOpener := binary.BigEndian.Uint32(cell[:3])
	agentOpened := binary.BigEndian.Uint32(cell[:7])
	return agentOpener, agentOpened
}

func main() {
	StartRouter("Tor61Router-4215-0001", 1238, 4611)
}

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

// Cell types
const (
	create       uint8 = 1
	created      uint8 = 2
	relayCell    uint8 = 3
	destroy      uint8 = 4
	open         uint8 = 5
	opened       uint8 = 6
	openFailed   uint8 = 7
	createFailed uint8 = 8
)

// Relay types
const (
	begin        uint8 = 1
	data         uint8 = 2
	end          uint8 = 3
	connected    uint8 = 4
	extend       uint8 = 6
	extended     uint8 = 7
	beginFailed  uint8 = 11
	extendFailed uint8 = 12
)

type circuit struct {
	circuitID uint16
	agentID   uint32
}

type relay struct {
	circuitID    uint16
	streamID     uint16
	digest       uint32
	bodyLength   uint16
	relayCommand uint8
	body         []byte
}

// Maps from router ID to a sending channel.
var currentConnections = make(map[uint32](chan []byte))

// Maps from a circuit to a reading channel.
var circuitToInput = make(map[circuit](chan []byte))

// Stores a value if we initiated the TCP connection to the agent.
var initiatedConnection = make(map[uint32]bool)

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
func acceptConnection(c net.Conn) {
	// New connection. First, wait for an "open".
	cell := make([]byte, 512)
	_, err := c.Read(cell)
	if err != nil {
		return
	}
	otherAgentID := uint32(0)
	// Maps from circuit ID to a message type.
	// For example, if we just sent a create message on
	// circuit ID 6, then 6 would map to type create
	// since we just sent that message and are waiting
	// on a reply.
	circuitID, cellType := cellCIDAndType(cell)
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
			c.Write(cell)
		}
	} else {
		// Connection initiated but they did not send an open cell.
		return
	}

	toSend := make(chan []byte, 100)
	currentConnections[otherAgentID] = toSend
	handleConnection(c, otherAgentID)
}

func handleConnection(c net.Conn, otherAgentID uint32) {
	defer c.Close()
	toSend := currentConnections[otherAgentID]
	// Thread that blocks on data to send and sends it.
	go func() {
		for {
			cell := <-toSend
			c.Write(cell)
		}
	}()
	// Thread that blocks on the socket waiting for data and puts
	// it in channel.
	// go func() {
	// 	for {
	// 		buffer := make([]byte, 512)
	// 		_, err := c.Read(buffer)
	// 		if err != nil {
	// 			continue
	// 		}
	// 		toRead <- buffer
	// 	}
	// }()
	for {
		// Wait for data to come on the channel then process it.
		cell := make([]byte, 512)
		_, err := c.Read(cell)
		if err != nil {
			continue
		}
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
				// Otherwise, create a channel to put that circuits data on
				// and add it to the routing table.
				circuitToInput[circuit{circuitID, otherAgentID}] = make(chan []byte, 100)
				routingTable[circuit{circuitID, otherAgentID}] = circuit{0, 0}
				cell[2] = created
				go watchChannel(circuit{circuitID, otherAgentID})
			}
			toSend <- cell
			continue
		default:
			circuitToInput[circuit{circuitID, otherAgentID}] <- cell
		}
		// TODO: If we receive a destroy and there are no circuits left
		// then we can close this connection potentially.

	}
}

func extendRelay(r relay, c circuit, endOfRelay bool, cell []byte) {
	// First, we want to check if we're the end of a relay.
	// If so, we want to extend the relay ourselves.
	if endOfRelay {
		address := string(r.body[:clen(r.body)])
		extendAgent := binary.BigEndian.Uint32(r.body[(clen(r.body) + 1):])
		agentConnection, ok := currentConnections[extendAgent]
		if !ok {
			// If there isn't an existing connection, we must create one.
			conn, err := net.Dial("tcp", address)
			if err != nil {
				currentConnections[c.agentID] <- createRelay(r.circuitID, r.streamID,
					r.digest, 0, extendFailed, nil)
				return
			}
			openCell := createOpenCell(createCell(0, open), routerID, extendAgent)
			conn.Write(openCell)
			reply := make([]byte, 512)
			_, err = conn.Read(cell)
			if err != nil {
				currentConnections[c.agentID] <- createRelay(r.circuitID, r.streamID,
					r.digest, 0, extendFailed, nil)
				return
			}
			_, replyType := cellCIDAndType(reply)
			if replyType == openFailed {
				currentConnections[c.agentID] <- createRelay(r.circuitID, r.streamID,
					r.digest, 0, extendFailed, nil)
				return
			}
			currentConnections[extendAgent] = make(chan []byte, 100)
			go handleConnection(conn, extendAgent)
			agentConnection = currentConnections[extendAgent]
			initiatedConnection[extendAgent] = true
			// At this point, we've opened a connection successfully.
		}
		// If there's an existing connection to the agent, we
		// put on the write buffer a create message with a new circuit
		// ID.
		_, odd := initiatedConnection[extendAgent]
		newCircuitID := uint16((2 * rand.Intn(500)))
		if odd {
			newCircuitID = newCircuitID + 1
		}
		// need to make sure this is a unique circuit number by checking
		// the map
		cell := createCell(newCircuitID, create)
		newCircuit := circuit{newCircuitID, extendAgent}
		circuitToInput[newCircuit] = make(chan []byte, 100)
		agentConnection <- cell
		reply := <-circuitToInput[newCircuit]
		_, replyType := cellCIDAndType(reply)
		if replyType == created {
			routingTable[c] = newCircuit
			routingTable[newCircuit] = c
			currentConnections[c.agentID] <- createRelay(r.circuitID, r.streamID,
				r.digest, 0, extended, nil)
			go watchChannel(newCircuit)
		} else {
			currentConnections[c.agentID] <- createRelay(r.circuitID, r.streamID,
				r.digest, 0, extendFailed, nil)
		}
	} else {
		// If we're not the end of the relay, just foward the message.
		endPoint := routingTable[c]
		binary.BigEndian.PutUint16(cell[:2], endPoint.circuitID)
		currentConnections[endPoint.agentID] <- cell
	}
}

// Handles all messages sent by agent A on circuit C.
func watchChannel(c circuit) {
	for {
		cell := <-circuitToInput[c]
		circuitID, cellType := cellCIDAndType(cell)
		nextHop := routingTable[c]
		endOfRelay := nextHop == circuit{0, 0}
		// The circuitID should already be known.
		switch cellType {
		case relayCell:
			r := parseRelay(cell)
			switch r.relayCommand {
			case extend:
				extendRelay(r, c, endOfRelay, cell)
			}
		}
	}
}

func createRelay(circuitID uint16, streamID uint16, digest uint32,
	bodyLength uint16, relayCommand uint8, body []byte) []byte {

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
		acceptConnection(c)
	}
}

func cellCIDAndType(cell []byte) (uint16, uint8) {
	return binary.BigEndian.Uint16(cell[:2]), cell[3]
}

func parseRelay(cell []byte) relay {
	circuitID, _ := cellCIDAndType(cell)
	streamID := binary.BigEndian.Uint16(cell[3:5])
	digest := binary.BigEndian.Uint32(cell[7:11])
	bodyLength := binary.BigEndian.Uint16(cell[11:13])
	relayCommand := cell[13]
	body := cell[14:(14 + bodyLength)]
	return relay{circuitID, streamID, digest, bodyLength, relayCommand, body}
}

func createCell(circuitID uint16, cellType uint8) []byte {
	header := make([]byte, 512)
	binary.BigEndian.PutUint16(header[:2], circuitID)
	header[2] = cellType
	return header
}

func createOpenCell(header []byte, agentOpener uint32, agentOpened uint32) []byte {
	binary.BigEndian.PutUint32(header[3:7], agentOpener)
	binary.BigEndian.PutUint32(header[7:11], agentOpened)
	return header
}

func readOpenCell(cell []byte) (uint32, uint32) {
	agentOpener := binary.BigEndian.Uint32(cell[3:7])
	agentOpened := binary.BigEndian.Uint32(cell[7:11])
	return agentOpener, agentOpened
}

func main() {
	StartRouter("Tor61Router-4215-0001", 1238, 4611)
}

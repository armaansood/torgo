package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
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

var circuitToStream = make(map[circuit](uint16))

// Stores a value if we initiated the TCP connection to the agent.
var initiatedConnection = make(map[uint32]bool)

// TODO: Use concurrent maps

// TODO: store a map/set of circuits where we're the last one

var firstHopCircuit circuit

var agent *r.Agent

var port uint16

// Bijective mapping from (circuitID, agentID) to (circuitID, agentID).
// Mapping from (circuitID, agentID) to (0, 0) means it is the
var routingTableForward = make(map[circuit]circuit)
var routingTableBackward = make(map[circuit]circuit)
var routerID uint32

var wg sync.WaitGroup

func cleanup() {
	fmt.Println("Cleaning up...")
	agent.Unregister("127.0.0.1", port)
}

func StartRouter(routerName string) {
	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		cleanup()
		os.Exit(1)
	}()
	agent = new(r.Agent)
	agent.StartAgent(regServerIP, regServerPort, true)
	agent.Register("127.0.0.1", port, routerID, uint8(len(routerName)), routerName)
	go localServer(port)
	rand.Seed(time.Now().Unix())
	responses := agent.Fetch("Tor61Router-4215")
	var circuitNodes []r.FetchResponse
	for i := 0; i < 3; i++ {
		circuitNodes = append(circuitNodes, responses[rand.Intn(len(responses))])
	}
	fmt.Println(circuitNodes)

	//
	//
	// First, we check if there's a connection to the first router.
	for !openConnectionIfNotExists(circuitNodes[0].IP+":"+strconv.Itoa(int(circuitNodes[0].Port)), circuitNodes[0].Data) {
		// If the connection failed, then use a different node.
		circuitNodes[0] = responses[rand.Intn(len(responses))]
		fmt.Println("Failed")
	}
	fmt.Println("Connection opened")
	agentConnection := currentConnections[circuitNodes[0].Data]
	_, odd := initiatedConnection[circuitNodes[0].Data]
	newCircuitID := uint16(2 * rand.Intn(500))
	if odd {
		newCircuitID = newCircuitID + 1
	}
	cell := createCell(newCircuitID, create)
	// Change this to firsthopcircuit
	firstCircuit := circuit{newCircuitID, circuitNodes[0].Data}
	circuitToInput[firstCircuit] = make(chan []byte, 100)
	agentConnection <- cell
	reply := <-circuitToInput[firstCircuit]
	_, replyType := cellCIDAndType(reply)
	if replyType != created {
		fmt.Println("Failed to create")
	} else {
		fmt.Println("Opened connection to first router")
	}
	fmt.Printf("Routing table forward: %+v, backward %+v\n", routingTableForward, routingTableBackward)
	fmt.Printf("Current connections: %+v\n", currentConnections)
	fmt.Printf("Initiated connection: %+v\n", initiatedConnection)
	fmt.Printf("First hop: %+v\n", firstHopCircuit)

	// Now we have a connection to one router and a circuit.
	// Now, we extend the relay.
	var body []byte
	address2 := circuitNodes[1].IP + ":" + strconv.Itoa(int(circuitNodes[1].Port))
	body = append(body, address2...)
	body = append(body, 0)
	fmt.Println(body)
	body = append(body, make([]byte, 4)...)
	fmt.Println(body)
	binary.BigEndian.PutUint32(body[len(address2)+1:], circuitNodes[1].Data)
	relay1 := createRelay(newCircuitID, 0, 0, uint16(len(body)), extend, body)
	fmt.Println(body)
	agentConnection <- relay1
	reply = <-circuitToInput[firstCircuit]
	relayReply := parseRelay(reply)
	if relayReply.relayCommand == extend && routerID == circuitNodes[0].Data {
		fmt.Println("EXTEND SELF")
		extendRelay(relayReply, firstCircuit, true, reply)
		reply = <-circuitToInput[firstCircuit]
		relayReply = parseRelay(reply)
	}
	if relayReply.relayCommand != extended {
		// Need to retry here
		fmt.Println("Failed to extend")
	} else {
		fmt.Println("Extended!!")
	}

	fmt.Printf("Routing table forward: %+v, back: %+v\n", routingTableForward, routingTableBackward)
	fmt.Printf("Current connections: %+v\n", currentConnections)
	fmt.Printf("Initiated connection: %+v\n", initiatedConnection)
	fmt.Printf("First hop: %+v\n", firstHopCircuit)

	//TODO: clean this mess up
	address3 := circuitNodes[2].IP + ":" + strconv.Itoa(int(circuitNodes[2].Port))
	var body2 []byte
	body2 = append(body2, address3...)
	body2 = append(body2, 0)
	body2 = append(body2, make([]byte, 4)...)
	binary.BigEndian.PutUint32(body2[len(address2)+1:], circuitNodes[2].Data)
	relay2 := createRelay(newCircuitID, 0, 0, uint16(len(body2)), extend, body2)
	agentConnection <- relay2
	reply = <-circuitToInput[firstCircuit]
	relayReply2 := parseRelay(reply)
	if relayReply2.relayCommand == extend && routerID == circuitNodes[0].Data {
		fmt.Println("EXTEND SELF2")
		extendRelay(relayReply, firstCircuit, false, reply)
		reply = <-circuitToInput[firstCircuit]
		relayReply2 = parseRelay(reply)
	}
	if relayReply2.relayCommand != extended {
		// Need to retry here
		fmt.Println("Failed to extend")
		fmt.Println(relayReply2.relayCommand)
		fmt.Println(reply)
	} else {
		fmt.Println("Extended twice")
	}
	watchChannel(firstCircuit)

}

//func createCircuit(theirAgentID uint32

func openConnectionIfNotExists(address string, theirAgentID uint32) bool {
	_, ok := currentConnections[theirAgentID]
	return ok || openConnection(address, theirAgentID)
}

func openConnection(address string, theirAgentID uint32) bool {
	tcpAddr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		fmt.Println(address)
		fmt.Println(address == "127.0.0.1:1238")
		fmt.Println([]byte(address))
		fmt.Println([]byte("127.0.0.1:1238"))
		fmt.Println(err)
		os.Exit(2)
	}
	conn, err := net.DialTCP("tcp", nil, tcpAddr)
	if err != nil {
		fmt.Println(err)
		fmt.Println(address)
		fmt.Println(currentConnections)
		fmt.Println(theirAgentID)
		fmt.Println("Error")
		return false
	}
	openCell := createOpenCell(createCell(0, open), routerID, theirAgentID)
	conn.Write(openCell)
	fmt.Printf("%d: Sending open message to %d\n", routerID, theirAgentID)
	reply := make([]byte, 512)
	_, err = conn.Read(reply)
	if err != nil {
		return false
	}
	_, replyType := cellCIDAndType(reply)
	if replyType != opened {
		return false
	}
	if _, ok := currentConnections[theirAgentID]; !ok {
		currentConnections[theirAgentID] = make(chan []byte, 100)
	}
	fmt.Println(conn)
	go handleConnection(conn, theirAgentID)
	initiatedConnection[theirAgentID] = true
	fmt.Printf("%d: Received opened from %d\n", routerID, theirAgentID)
	return true
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
	fmt.Println("Waiting on data")
	_, err := c.Read(cell)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println("Received some data!")
	otherAgentID := uint32(0)
	// Maps from circuit ID to a message type.
	// For example, if we just sent a create message on
	// circuit ID 6, then 6 would map to type create
	// since we just sent that message and are waiting
	// on a reply.
	_, cellType := cellCIDAndType(cell)
	if cellType == open {
		agentOpened := uint32(0)
		otherAgentID, agentOpened = readOpenCell(cell)
		if agentOpened != routerID {
			// Open cell sent with wrong router ID.
			cell[2] = openFailed
			c.Write(cell)
			fmt.Println("Open failed")
			c.Close()
			return
		} else {
			// Opened connection.
			cell[2] = opened
			fmt.Println("Opened connection")
			c.Write(cell)
		}
	} else {
		fmt.Println("Bad message")
		c.Close()
		// Connection initiated but they did not send an open cell.
		return
	}
	// If it's a self-connection, we're already listening to ourselves.
	if _, ok := currentConnections[otherAgentID]; !ok {
		currentConnections[otherAgentID] = make(chan []byte, 100)
	}
	handleConnection(c, otherAgentID)
}

func handleConnection(c net.Conn, otherAgentID uint32) {
	defer c.Close()
	toSend := currentConnections[otherAgentID]
	fmt.Printf("ToSend: %+v conn: %+v\n", toSend, c)
	// Thread that blocks on data to send and sends it.
	go func() {
		for {
			cell := <-toSend
			circID, mType := cellCIDAndType(cell)
			fmt.Printf("%d: Sending message to %d of type %d on circuit %d\n", routerID, otherAgentID, mType, circID)
			fmt.Println(routingTableForward)
			fmt.Println(routingTableBackward)
			fmt.Println(circuitToStream)
			fmt.Println(firstHopCircuit)
			c.Write(cell)
		}
	}()
	for {
		// Wait for data to come on the channel then process it.
		cell := make([]byte, 512)
		_, err := c.Read(cell)
		if err != nil {
			fmt.Println(err)
			fmt.Printf("Error on connection from %d\n", otherAgentID)
			return
		}
		circuitID, cellType := cellCIDAndType(cell)
		fmt.Printf("%d: Received message from %d on circuit %d of type %d\n", routerID, otherAgentID, circuitID, cellType)
		switch cellType {
		case create:
			// Other agent wants to create a new circuit.
			// Need to ensure that this circuit ID is unique.
			// We are now the endpoint of some circuit.
			_, ok := routingTableForward[circuit{circuitID, otherAgentID}]
			if ok {
				// Circuit already existed.
				fmt.Println("Circuit already existed!")
				cell[2] = createFailed
			} else {
				// Otherwise, create a channel to put that circuits data on
				// and add it to the routing table.
				_, ok := circuitToInput[circuit{circuitID, otherAgentID}]
				if !ok {
					// Make sure it doesn't already exist.
					fmt.Println("Making a new one")
					circuitToInput[circuit{circuitID, otherAgentID}] = make(chan []byte, 100)
				}
				//				routingTable[circuit{circuitID, otherAgentID}] = circuit{0, 0}
				circuitToStream[circuit{circuitID, otherAgentID}] = 0
				cell[2] = created
				if !ok {
					fmt.Println("WATCHING CHAHNNEL")
					// If not, someone is already watching the channel (self)
					go watchChannel(circuit{circuitID, otherAgentID})
				}
				fmt.Println("Watching chanenl")
			}
			toSend <- cell
			continue
		default:
			fmt.Println("Noncreate received")
			circuitToInput[circuit{circuitID, otherAgentID}] <- cell
		}
		// TODO: If we receive a destroy and there are no circuits left
		// then we can close this connection potentially.

	}
}

// r is the relay sent on circuit c.o
func extendRelay(r relay, c circuit, endOfRelay bool, cell []byte) {
	// First, we want to check if we're the end of a relay.
	// If so, we want to extend the relay ourselves.
	fmt.Println("EXTENDING")
	if endOfRelay {
		fmt.Println("extending now")
		fmt.Println(r.body)
		address := string(r.body[:clen(r.body)])
		fmt.Println(r.body)
		fmt.Println(r.body[:clen(r.body)+1])
		fmt.Println(address)
		fmt.Println("HERE")
		extendAgent := binary.BigEndian.Uint32(r.body[(clen(r.body))+1:])
		fmt.Println(extendAgent)
		agentConnection, ok := currentConnections[extendAgent]
		if !ok {
			// If there isn't an existing connection, we create one.
			if !openConnection(address, extendAgent) {
				currentConnections[c.agentID] <- createRelay(r.circuitID, r.streamID,
					r.digest, 0, extendFailed, nil)
				return
			}
			agentConnection = currentConnections[extendAgent]
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
		if _, ok := circuitToInput[newCircuit]; !ok {
			fmt.Println("creating 2")
			circuitToInput[newCircuit] = make(chan []byte, 100)
		}
		agentConnection <- cell
		reply := <-circuitToInput[newCircuit]
		_, replyType := cellCIDAndType(reply)
		if replyType == created {
			fmt.Println("Routing %+v to %+v\n", c, newCircuit)
			routingTableForward[c] = newCircuit
			routingTableBackward[newCircuit] = c
			delete(circuitToStream, c)
			fmt.Println(routingTableForward)
			fmt.Println(routingTableBackward)
			fmt.Println("new routing taqble")
			currentConnections[c.agentID] <- createRelay(r.circuitID, r.streamID,
				r.digest, 0, extended, nil)
			fmt.Println(newCircuit)
			go watchChannel(newCircuit)
		} else {
			fmt.Println(currentConnections)
			fmt.Printf("about to send it to %d\n", c.agentID)
			currentConnections[c.agentID] <- createRelay(r.circuitID, r.streamID,
				r.digest, 0, extendFailed, nil)
		}
	} else {
		// If we're not the end of the relay, just foward the message.
		endPoint := routingTableForward[c]
		fmt.Println(routingTableForward)
		fmt.Printf("Forwarding message to %+v\n", endPoint)
		binary.BigEndian.PutUint16(cell[:2], endPoint.circuitID)
		currentConnections[endPoint.agentID] <- cell
	}
}

// Handles all messages sent by agent A on circuit C.
func watchChannel(c circuit) {
	for {
		cell := <-circuitToInput[c]
		fmt.Printf("%d: Cell received on circuit %+v\n", routerID, c)
		_, cellType := cellCIDAndType(cell)
		endOfRelay := false
		if _, ok := circuitToStream[c]; ok {
			endOfRelay = true
		}
		// The circuitID should already be known.
		switch cellType {
		case relayCell:
			r := parseRelay(cell)
			switch r.relayCommand {
			case extend:
				fmt.Println("Extend received!")
				extendRelay(r, c, endOfRelay, cell)
				//	case begin:
			case extended:
				fmt.Println("Extended a circuit")
				previousCircuit := routingTableBackward[c]
				binary.BigEndian.PutUint16(cell[:2], previousCircuit.circuitID)
				fmt.Println(cell)
				currentConnections[previousCircuit.agentID] <- cell

			}

			//			case destroy:
		}
	}
}

func createRelay(circuitID uint16, streamID uint16, digest uint32,
	bodyLength uint16, relayCommand uint8, body []byte) []byte {
	cell := createCell(circuitID, relayCell)
	binary.BigEndian.PutUint16(cell[3:5], streamID)
	binary.BigEndian.PutUint32(cell[7:11], digest)
	binary.BigEndian.PutUint16(cell[11:13], bodyLength)
	cell[13] = relayCommand
	for i := uint16(0); i < bodyLength; i++ {
		cell[i+14] = body[i]
	}
	return cell
}

func localServer(port uint16) {
	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(int(port)))
	if err != nil {
		os.Exit(2)
	}
	defer l.Close()

	for {
		fmt.Printf("Listening on port %d\n", port)
		c, err := l.Accept()
		if err != nil {
			fmt.Println(err)
			fmt.Println("SERVER ERROR")
			continue
		} else {
			fmt.Println("Connection accepted")
		}
		go acceptConnection(c)
	}
}

func cellCIDAndType(cell []byte) (uint16, uint8) {
	return binary.BigEndian.Uint16(cell[:2]), cell[2]
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
	if len(os.Args) != 4 {
		usage()
	}
	flag.Parse()
	group, err := strconv.Atoi(flag.Arg(0))
	if err != nil {
		usage()
	}
	instance, err := strconv.Atoi(flag.Arg(1))
	if err != nil {
		usage()
	}
	proxyPort, err := strconv.Atoi(flag.Arg(2))
	if err != nil {
		usage()
	}
	routerName := "Tor61Router-" + fmt.Sprintf("%04d", group) + "-" + fmt.Sprintf("%04d", instance)
	routerNum := (group << 16) | (instance)
	fmt.Println(routerName)
	fmt.Println(routerID)
	port = uint16(proxyPort)
	routerID = uint32(routerNum)
	StartRouter(routerName)
}

func usage() {
	fmt.Println("Usage: ./run <group number> <instance number> <HTTP Proxy port>")
	os.Exit(2)
}

func clen(n []byte) int {
	for i := 0; i < len(n); i++ {
		if n[i] == 0 {
			fmt.Println(i)
			return i
		}
	}
	return len(n)
}

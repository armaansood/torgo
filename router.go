package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"os"
	"strconv"
	"sync"
	"time"
	p "torgo/proxy"
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

const bufferSize int = 100
const circuitLength int = 3

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

var circuitToReply = make(map[circuit](chan []byte))

// incorrect, can have multiple streams per circuit
var circuitToStream = make(map[circuit](uint16))

var streamToProxyReceiver = make(map[uint16](chan []byte))
var streamToServerReceiver = make(map[uint16](chan []byte))

// Stores a value if we initiated the TCP connection to the agent.
var initiatedConnection = make(map[uint32]bool)

// TODO: Use concurrent maps

// TODO: store a map/set of circuits where we're the last one

var firstCircuit circuit

var agent *r.Agent

var proxyPort uint16
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

func deleteRouter(targetRouterID uint32) {
	for k, v := range routingTableForward {
		if k.agentID == targetRouterID || v.agentID == targetRouterID {
			//close(circuitToInput[k])
			//close(circui
			delete(routingTableForward, k)
			delete(routingTableBackward, v)
			delete(circuitToInput, k)
			delete(circuitToInput, v)
			delete(circuitToStream, k)
			delete(circuitToStream, v)
		}
		delete(currentConnections, targetRouterID)
		delete(initiatedConnection, targetRouterID)
	}
}

func StartRouter(routerName string) {
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {

		}
		cleanup()
		os.Exit(1)
	}()
	agent = new(r.Agent)
	agent.StartAgent(regServerIP, regServerPort, false)
	for !(agent.Register("127.0.0.1", port, routerID, uint8(len(routerName)), routerName)) {
		fmt.Println("Could not register self! Retrying...")
	}
	fmt.Println("Registered router %d on port %d\n", routerID, port)
	rand.Seed(time.Now().Unix())
	var responses []r.FetchResponse
	for len(responses) == 0 {
		fmt.Println("Fetching responses...")
		responses = agent.Fetch("Tor61Router-4215")
	}
	var circuitNodes []r.FetchResponse
	fmt.Println(responses)
	for i := 0; i < circuitLength; i++ {
		circuitNodes = append(circuitNodes, responses[rand.Intn(len(responses))])
	}
	fmt.Println(circuitNodes)

	firstCircuit = circuit{0, 0}
	for (firstCircuit == circuit{0, 0}) {
		firstCircuit = createCircuit(circuitNodes[0].Data,
			circuitNodes[0].IP+":"+strconv.Itoa(int(circuitNodes[0].Port)))
		// If this node failed, try another.
		if (firstCircuit == circuit{0, 0}) {
			fmt.Println("Failed to connect to %d\n", circuitNodes[0].Data)
			circuitNodes[0] = responses[rand.Intn(len(responses))]
		}
	}

	firstHopConnection := currentConnections[firstCircuit.agentID]
	go watchChannel(firstCircuit)
	fmt.Printf("Routing table forward: %+v, backward %+v\n", routingTableForward, routingTableBackward)
	fmt.Printf("Current connections: %+v\n", currentConnections)
	fmt.Printf("Initiated connection: %+v\n", initiatedConnection)
	fmt.Printf("First hop circuit: %+v\n", firstCircuit)
	fmt.Printf("Streams: %+v\n\n", circuitToStream)
	// Now we have a connection to one router and a circuit.
	// Now, we extend the relay.

	for i := 1; i < circuitLength; i++ {
		var body []byte
		address := circuitNodes[i].IP + ":" + strconv.Itoa(int(circuitNodes[i].Port))
		body = append(body, address...)
		body = append(body, 0)
		body = append(body, make([]byte, 4)...)
		binary.BigEndian.PutUint32(body[len(address)+1:], circuitNodes[i].Data)
		relay := createRelay(firstCircuit.circuitID, 0, 0, uint16(len(body)), extend, body)
		firstHopConnection <- relay
		//		reply := <-circuitToInput[firstCircuit]
		reply := <-circuitToReply[firstCircuit]
		relayReply := parseRelay(reply)
		// if relayReply.relayCommand == extend && routerID == circuitNodes[0].Data {
		// 	// This is the case where the first router in the path is ours.
		// 	// For example, A-->A-->B
		// 	// We are the only thread watching the first circuit (since we're waiting for
		// 	// replies like "created").
		// 	// Even if A == B, it's still OK because there is another thread listening
		// 	// on that circuit.
		// 	extendRelay(relayReply, firstCircuit, true, reply)
		// 	reply = <-circuitToInput[firstCircuit]
		// 	relayReply = parseRelay(reply)
		// }
		if relayReply.relayCommand != extended {
			fmt.Printf("Failed to extend to router %d\n", circuitNodes[i].Data)
			circuitNodes[i] = responses[rand.Intn(len(responses))]
			fmt.Printf("Retrying with router %d\n", circuitNodes[i].Data)
			i--
		} else {
			fmt.Printf("Successfully extended to %d\n\n", circuitNodes[i].Data)
		}
		fmt.Printf("Routing table forward: %+v, backward %+v\n", routingTableForward, routingTableBackward)
		fmt.Printf("Current connections: %+v\n", currentConnections)
		fmt.Printf("Initiated connection: %+v\n", initiatedConnection)
		fmt.Printf("First hop circuit: %+v\n", firstCircuit)
		fmt.Printf("Streams: %+v\n\n", circuitToStream)
	}
	proxyServer(proxyPort)

}

// Creates a circuit between this router and the target router.
// Returns {0, 0} on failure.
func createCircuit(targetRouterID uint32, address string) circuit {
	for !openConnectionIfNotExists(address, targetRouterID) {
		fmt.Printf("Failed to open connection to %d\n", targetRouterID)
		return circuit{0, 0}
	}
	fmt.Printf("Connection opened to %d\n", targetRouterID)
	targetConnection := currentConnections[targetRouterID]
	_, odd := initiatedConnection[targetRouterID]
	newCircuitID := uint16(2 * rand.Intn(100000))
	if odd {
		newCircuitID = newCircuitID + 1
	}

	// In the rare case that there is already a circuit between this router
	// and the target with the random number that was just generated, retry.
	if _, ok := circuitToInput[circuit{newCircuitID, targetRouterID}]; ok {
		newCircuitID = uint16(2 * rand.Intn(100000))
		if odd {
			newCircuitID = newCircuitID + 1
		}
	}

	cell := createCell(newCircuitID, create)
	result := circuit{newCircuitID, targetRouterID}
	if _, ok := circuitToInput[result]; !ok {
		circuitToInput[result] = make(chan []byte, bufferSize)
	}
	targetConnection <- cell
	reply := <-circuitToInput[result]
	_, replyType := cellCIDAndType(reply)
	if replyType != created {
		fmt.Printf("Failed to create circuit %+v\n", result)
		delete(circuitToInput, result)
		return circuit{0, 0}
	}
	fmt.Println("Created circuit %+v\n", result)
	return result
}

func openConnectionIfNotExists(address string, targetRouterID uint32) bool {
	_, ok := currentConnections[targetRouterID]
	return ok || openConnection(address, targetRouterID)
}

// Opens a connection to target router with address.
func openConnection(address string, targetRouterID uint32) bool {
	conn, err := net.Dial("tcp", address)
	if err != nil {
		fmt.Println(err)
		return false
	}
	openCell := createOpenCell(routerID, targetRouterID)
	conn.Write(openCell)
	fmt.Printf("Sending open message to %d\n", targetRouterID)
	reply := make([]byte, 512)
	_, err = conn.Read(reply)
	if err != nil {
		return false
	}
	_, replyType := cellCIDAndType(reply)
	if replyType != opened {
		return false
	}
	if _, ok := currentConnections[targetRouterID]; !ok {
		currentConnections[targetRouterID] = make(chan []byte, 100)
	}
	initiatedConnection[targetRouterID] = true
	go handleConnection(conn, targetRouterID)
	fmt.Printf("Received opened from %d\n", targetRouterID)
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
func acceptConnection(conn net.Conn) {
	// New connection. First, wait for an "open".
	cell := make([]byte, 512)
	_, err := conn.Read(cell)
	if err != nil {
		conn.Close()
		fmt.Println(err)
		return
	}
	targetRouterID := uint32(0)
	_, cellType := cellCIDAndType(cell)
	if cellType == open {
		agentOpened := uint32(0)
		targetRouterID, agentOpened = readOpenCell(cell)
		if agentOpened != routerID {
			// Open cell sent with wrong router ID.
			cell[2] = openFailed
			conn.Write(cell)
			fmt.Println("Open failed - wrong router ID.")
			conn.Close()
			return
		} else {
			// Opened connection.
			fmt.Printf("Opened connection with %d\n", targetRouterID)
			cell[2] = opened
			conn.Write(cell)
		}
	} else {
		conn.Close()
		fmt.Println("Open failed - invalid cell sent.")
		return
	}
	// If it's a self-connection, we're already listening to ourselves.
	if _, ok := currentConnections[targetRouterID]; !ok {
		fmt.Printf("Creating connection channel with %d\n", targetRouterID)
		currentConnections[targetRouterID] = make(chan []byte, 100)
	}
	handleConnection(conn, targetRouterID)
}

func handleConnection(conn net.Conn, targetRouterID uint32) {
	toSend := currentConnections[targetRouterID]
	defer conn.Close()
	defer close(toSend)
	defer delete(currentConnections, targetRouterID)
	// Thread that blocks on data to send and sends it.
	go func() {
		for {
			cell := <-toSend
			circID, mType := cellCIDAndType(cell)
			fmt.Printf("%d: Sending message to %d of type %d on circuit %d\n", routerID, targetRouterID, mType, circID)
			// fmt.Printf("Routing table forward: %+v, backward %+v\n", routingTableForward, routingTableBackward)
			// fmt.Printf("Current connections: %+v\n", currentConnections)
			// fmt.Printf("Initiated connection: %+v\n", initiatedConnection)
			// fmt.Printf("First hop circuit: %+v\n", firstCircuit)
			// fmt.Printf("Streams: %+v\n", circuitToStream)
			conn.Write(cell)
		}
	}()
	for {
		// Wait for data to come on the channel then process it.
		cell := make([]byte, 512)
		_, err := conn.Read(cell)
		if err != nil {
			deleteRouter(targetRouterID)
			fmt.Println(err)
			fmt.Printf("Error on connection from %d\n", targetRouterID)
			return
		}
		circuitID, cellType := cellCIDAndType(cell)
		fmt.Printf("%d: Received message from %d on circuit %d of type %d\n", routerID, targetRouterID, circuitID, cellType)
		switch cellType {
		case create:
			// Other agent wants to create a new circuit.
			// Need to ensure that this circuit ID is unique.
			// We are now the endpoint of some circuit.
			_, ok := routingTableForward[circuit{circuitID, targetRouterID}]
			if ok {
				// Circuit already existed.
				fmt.Println("Circuit already existed!")
				cell[2] = createFailed
			} else {
				// Otherwise, create a channel to put that circuits data on
				// and add it to the routing table.
				_, ok := circuitToInput[circuit{circuitID, targetRouterID}]
				circuitToStream[circuit{circuitID, targetRouterID}] = 0
				cell[2] = created
				if !ok {
					// Make sure it doesn't already exist.
					circuitToInput[circuit{circuitID, targetRouterID}] = make(chan []byte, 100)
					// If not, someone is already watching the channel (self)
					go watchChannel(circuit{circuitID, targetRouterID})
				}
			}
			toSend <- cell
			continue
		default:
			circuitToInput[circuit{circuitID, targetRouterID}] <- cell
		}
		// TODO: If we receive a destroy and there are no circuits left
		// then we can close this connection potentially.
	}
}

// r is the relay sent on circuit c.
// endOfRelay indicates whether this router is the last stop in the relay.
func extendRelay(r relay, c circuit, endOfRelay bool, cell []byte) {
	if endOfRelay {
		address := string(r.body[:clen(r.body)])
		targetRouterID := binary.BigEndian.Uint32(r.body[(clen(r.body))+1:])
		result := createCircuit(targetRouterID, address)
		if (result != circuit{0, 0}) {
			routingTableForward[c] = result
			routingTableBackward[result] = c
			delete(circuitToStream, c)
			currentConnections[c.agentID] <- createRelay(r.circuitID, r.streamID,
				r.digest, 0, extended, nil)
			go watchChannel(result)
		} else {
			currentConnections[c.agentID] <- createRelay(r.circuitID, r.streamID,
				r.digest, 0, extendFailed, nil)
		}
	} else {
		// If we're not the end of the relay, just foward the message.
		endPoint := routingTableForward[c]
		fmt.Printf("Forwarding message to %+v\n", endPoint)
		binary.BigEndian.PutUint16(cell[:2], endPoint.circuitID)
		currentConnections[endPoint.agentID] <- cell
	}
}

// Handles all messages sent by agent A on circuit C.
func watchChannel(c circuit) {
	circuitToReply[c] = make(chan []byte, bufferSize)
	for {
		cell := <-circuitToInput[c]
		fmt.Printf("Cell received on circuit %+v\n", c)
		_, cellType := cellCIDAndType(cell)
		_, endOfRelay := circuitToStream[c]
		// The circuitID should already be known.
		switch cellType {
		case relayCell:
			r := parseRelay(cell)
			switch r.relayCommand {
			case extend:
				extendRelay(r, c, endOfRelay, cell)
			case begin:
				beginRelay(r, c, endOfRelay, cell)
			case data:
				previousCircuit, back := routingTableBackward[c]
				nextCircuit, front := routingTableForward[c]
				if back {
					// If the circuit is travelling backwards.
					binary.BigEndian.PutUint16(cell[:2], previousCircuit.circuitID)
					currentConnections[previousCircuit.agentID] <- cell
				} else if front {
					binary.BigEndian.PutUint16(cell[:2], nextCircuit.circuitID)
					currentConnections[nextCircuit.agentID] <- cell
				} else {
					// This must be the endpoint.
					//	streamToReceiver[r.streamID] <- r.body
				}
			default:
				// Connected, extended, begin failed, extend failed
				previousCircuit, ok := routingTableBackward[c]
				if !ok {
					circuitToReply[c] <- cell
				} else {
					binary.BigEndian.PutUint16(cell[:2], previousCircuit.circuitID)
					currentConnections[previousCircuit.agentID] <- cell
				}
			}

			//			case destroy:
		}
	}
}

func beginRelay(r relay, c circuit, endOfRelay bool, cell []byte) {
	if !endOfRelay {
		endPoint := routingTableForward[c]
		fmt.Printf("Forwarding begin to %+v\n", endPoint)
		binary.BigEndian.PutUint16(cell[:2], endPoint.circuitID)
		currentConnections[endPoint.agentID] <- cell
		return
	}
	address := string(r.body[:clen(r.body)])
	conn, err := net.Dial("tcp", address)
	if err != nil {
		currentConnections[c.agentID] <- createRelay(r.circuitID, r.streamID,
			r.digest, 0, beginFailed, nil)
		return
	}
	circuitToStream[c] = r.streamID
	//	streamToReceiver[r.streamID]
	go handleStreamEnd(conn, r.streamID)
	currentConnections[c.agentID] <- createRelay(r.circuitID, r.streamID,
		r.digest, 0, connected, nil)
	fmt.Printf("Created stream %d at %d\n", r.streamID, routerID)
}

func handleStreamEnd(conn net.Conn, streamID uint16) {
	defer conn.Close()

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

func routerServer(l net.Listener) {
	defer l.Close()
	for {
		c, err := l.Accept()
		if err != nil {
			fmt.Println(err)
			continue
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

func createOpenCell(agentOpener uint32, agentOpened uint32) []byte {
	header := createCell(0, open)
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
	proxy, err := strconv.Atoi(flag.Arg(2))
	if err != nil {
		usage()
	}
	routerName := "Tor61Router-" + fmt.Sprintf("%04d", group) + "-" + fmt.Sprintf("%04d", instance)
	routerNum := (group << 16) | (instance)
	fmt.Println(routerName)
	fmt.Println(routerID)
	proxyPort = uint16(proxy)
	routerID = uint32(routerNum)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println(err)
		os.Exit(2)
	}
	port = uint16(l.Addr().(*net.TCPAddr).Port)
	go routerServer(l)
	StartRouter(routerName)
}

func createStream(streamID uint16, address string) bool {
	var body []byte
	body = append(body, address...)
	body = append(body, 0)
	relay := createRelay(firstCircuit.circuitID, streamID, 0, uint16(len(body)),
		begin, body)
	currentConnections[firstCircuit.agentID] <- relay
	reply := <-circuitToReply[firstCircuit]
	relayReply := parseRelay(reply)
	if relayReply.relayCommand != connected {
		fmt.Printf("Failed to connect!!")
		return false
	}
	fmt.Printf("Connected to %s on stream %d\n", address, streamID)
	return true
}

func proxyServer(port uint16) {
	l, err := net.Listen("tcp", "0.0.0.0:"+strconv.Itoa(int(port)))
	if err != nil {
		fmt.Println(err)
		os.Exit(2)
	}
	defer l.Close()
	for {
		conn, err := l.Accept()
		if err != nil {
			fmt.Println(err)
			continue
		}
		go handleProxyConnection(conn)
	}
}

func handleProxyConnection(conn net.Conn) {
	header := p.ParseHTTPRequest(conn)
	// need to make sure this is a unique stream.
	streamID := uint16(rand.Intn(100000))
	if createStream(uint16(rand.Intn(100000)), header.IP+":"+header.Port) {
		//	streamToReceiver[streamID] = make(chan []byte, bufferSize)
		// Now that we have a stream, we send over the initial request via a data
		// message.
		dataRelay := createRelay(firstCircuit.circuitID, streamID, 0,
			uint16(len(header.Data)), data, header.Data)
		currentConnections[firstCircuit.agentID] <- dataRelay
		reply := <-circuitToReply[firstCircuit]
		conn.Write(reply)
	}
}

func usage() {
	fmt.Println("Usage: ./run <group number> <instance number> <HTTP Proxy port>")
	os.Exit(2)
}

func clen(n []byte) int {
	for i := 0; i < len(n); i++ {
		if n[i] == 0 {
			return i
		}
	}
	return len(n)
}

package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	mapuint16 "torgo/concurrent_uint16map"
	mapuint32 "torgo/concurrent_uint32map"
	p "torgo/proxy"
	r "torgo/regagent"
)

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

const bufferSize int = 10000
const sendBufferSize int = 10000000
const circuitLength int = 3
const maxDataSize int = 497

type circuit struct {
	circuitID uint16
	routerID  uint32
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
var currentConnections = mapuint32.New()

func currentConnectionsRead(routerID uint32) chan []byte {
	result, _ := currentConnections.Get(routerID)
	if result == nil {
		return nil
	}
	return result.(chan []byte)
}

// Maps from a circuit to a reading channel.
var circuitToInput = make(map[circuit](chan []byte))
var circuitToInputLock = sync.RWMutex{}

func circuitToInputRead(c circuit) (chan []byte, bool) {
	circuitToInputLock.RLock()
	defer circuitToInputLock.RUnlock()
	result, ok := circuitToInput[c]
	return result, ok
}

func circuitToInputWrite(c circuit, channel chan []byte) {
	circuitToInputLock.Lock()
	defer circuitToInputLock.Unlock()
	circuitToInput[c] = channel
}

var circuitToReply = make(map[circuit](chan []byte))
var circuitToReplyLock = sync.RWMutex{}

var circuitToIsEnd = make(map[circuit](bool))
var circuitToIsEndLock = sync.RWMutex{}

func circuitToIsEndRead(c circuit) (bool, bool) {
	circuitToIsEndLock.RLock()
	defer circuitToIsEndLock.RUnlock()
	result, ok := circuitToIsEnd[c]
	return result, ok
}

func circuitToIsEndWrite(c circuit, value bool) {
	circuitToIsEndLock.Lock()
	defer circuitToIsEndLock.Unlock()
	circuitToIsEnd[c] = value
}

var streamToReceiverLock = sync.RWMutex{}
var streamToReceiver = mapuint16.New()

func streamToReceiverRead(streamID uint16) chan []byte {
	result, ok := streamToReceiver.Get(streamID)
	if !ok {
		return nil
	}
	return result.(chan []byte)
}

// Stores a value if we initiated the TCP connection to the agent.
var initiatedConnection = make(map[uint32]bool)
var initiatedConnectionLock = sync.RWMutex{}

var firstCircuit circuit

var agent *r.Agent

var proxyPort uint16
var port uint16
var ip string
var routersToFetch string

var ipToRegister = getOutboundIP()

// Bijective mapping from (circuitID, routerID) to (circuitID, routerID).
// Mapping from (circuitID, routerID) to (0, 0) means it is the
var routingTableForward = make(map[circuit]circuit)
var routingTableBackward = make(map[circuit]circuit)
var routingTableLock = sync.RWMutex{}

var routerID uint32

var wg sync.WaitGroup

func cleanup() {
	circuits := getAllCircuitsOnThisRouter()
	for _, c := range circuits {
		sendCellToAgent(c.routerID, createCell(c.circuitID, destroy))
	}
}

func getAllCircuitsOnThisRouter() []circuit {
	circuitToInputLock.RLock()
	defer circuitToInputLock.RUnlock()
	result := make([]circuit, len(circuitToInput))
	i := 0
	for k := range circuitToInput {
		result[i] = k
		i++
	}
	return result
}

func deleteRouter(targetRouterID uint32) {
	for _, c := range getAllCircuitsOnThisRouter() {
		if c.routerID == targetRouterID {
			destroyCircuit(c)
		}
	}
	initiatedConnectionLock.Lock()
	delete(initiatedConnection, targetRouterID)
	initiatedConnectionLock.Unlock()
	currentConnections.Remove(targetRouterID)
}

func StartRouter(routerName string, group int, address string) {
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
		}
		go cleanup()
		fmt.Printf("\nUnregistering...\n")
		agent.Unregister(ipToRegister, port)
		fmt.Println("Shutting down router in 3 seconds...")
		fmt.Printf("Routing table forward: %+v, backward %+v\n", routingTableForward, routingTableBackward)
		fmt.Printf("Initiated connection: %+v\n", initiatedConnection)
		fmt.Printf("First hop circuit: %+v\n", firstCircuit)
		d := streamToReceiver.Keys()
		fmt.Printf("Stream to data: %+v\n\n", d)
		time.Sleep(3 * time.Second)
		os.Exit(1)
	}()

	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		fmt.Println(err)
		os.Exit(2)
	}
	port = uint16(l.Addr().(*net.TCPAddr).Port)
	fmt.Printf("Registering on %s with port %d\n", ipToRegister, port)
	addressSplit := strings.Split(address, ":")
	regServerIP := addressSplit[0]
	regServerPort := addressSplit[1]
	agent = new(r.Agent)
	agent.StartAgent(regServerIP, regServerPort, false)
	fmt.Printf("Attempting to register...\n")
	for !(agent.Register(ipToRegister, port, routerID,
		uint8(len(routerName)), routerName)) {
		fmt.Println("Could not register self! Retrying...")
		time.Sleep(3 * time.Second)
	}
	fmt.Printf("Registered router %d!\n\n", routerID)
	rand.Seed(time.Now().Unix())
	routersToFetch = "Tor61Router-" + string(fmt.Sprintf("%04d", group))
	go routerServer(l)
	createInitialCircuit()
	proxyServer(proxyPort)
}

func createInitialCircuit() {
	var responses []r.FetchResponse
	fmt.Printf("Fetching routers that begin with %s...\n", routersToFetch)
	responses = agent.Fetch(routersToFetch)
fetch:
	for len(responses) < 2 {
		fmt.Println("No other routers online. Waiting 3 seconds and retrying...")
		time.Sleep(3 * time.Second)
		responses = agent.Fetch(routersToFetch)
	}
	var circuitNodes []r.FetchResponse
	potentialFirstNode := responses[rand.Intn(len(responses))]
	for potentialFirstNode.Data == routerID {
		potentialFirstNode = responses[rand.Intn(len(responses))]
	}
	potentialLastNode := responses[rand.Intn(len(responses))]
	for potentialLastNode.Data == routerID {
		potentialLastNode = responses[rand.Intn(len(responses))]
	}
	circuitNodes = append(circuitNodes, potentialFirstNode)
	for i := 1; i < circuitLength-1; i++ {
		circuitNodes = append(circuitNodes, responses[rand.Intn(len(responses))])
	}
	circuitNodes = append(circuitNodes, potentialLastNode)
	firstCircuit = circuit{0, 0}
	for (firstCircuit == circuit{0, 0}) {
		fmt.Println("Attempting to create first hop circuit...")
		firstCircuit = createCircuit(circuitNodes[0].Data,
			circuitNodes[0].IP+":"+strconv.Itoa(int(circuitNodes[0].Port)))
		// If this node failed, try another.
		if (firstCircuit == circuit{0, 0}) {
			fmt.Printf("Failed to connect to %d\n", circuitNodes[0].Data)
			for i, v := range responses {
				if v == circuitNodes[0] {
					responses[i] = responses[len(responses)-1]
				}
			}
			responses = responses[:len(responses)-1]
			if len(responses) < 2 {
				goto fetch
			}
			circuitNodes[0] = responses[rand.Intn(len(responses))]
			for circuitNodes[0].Data == routerID {
				circuitNodes[0] = responses[rand.Intn(len(responses))]
			}
			fmt.Printf("Retrying with router %d\n", circuitNodes[0].Data)
		}
	}
	fmt.Printf("Created first hop circuit: %+v\n", firstCircuit)
	firstHopConnection := currentConnectionsRead(firstCircuit.routerID)
	circuitToReplyLock.Lock()
	circuitToReply[firstCircuit] = make(chan []byte, bufferSize)
	circuitToReplyLock.Unlock()
	go watchChannel(firstCircuit)
	for i := 1; i < circuitLength; i++ {
		fmt.Println("Attempting to extend circuit...")
		var body []byte
		address := circuitNodes[i].IP + ":" + strconv.Itoa(int(circuitNodes[i].Port))
		body = append(body, address...)
		body = append(body, 0)
		body = append(body, make([]byte, 4)...)
		binary.BigEndian.PutUint32(body[len(address)+1:], circuitNodes[i].Data)
		relay := createRelay(firstCircuit.circuitID, 0, 0, uint16(len(body)), extend, body)
		firstHopConnection <- relay
		circuitToReplyLock.RLock()
		waitChan := circuitToReply[firstCircuit]
		circuitToReplyLock.RUnlock()
		reply := <-waitChan
		relayReply := parseRelay(reply)
		if relayReply.relayCommand != extended {
			fmt.Printf("Failed to extend to router %d\n", circuitNodes[i].Data)
			circuitNodes[i] = responses[rand.Intn(len(responses))]
			if i == circuitLength-1 {
				for circuitNodes[i].Data == routerID {
					circuitNodes[i] = responses[rand.Intn(len(responses))]
				}
			}
			fmt.Printf("Retrying with router %d\n", circuitNodes[i].Data)
			i--
		} else {
			fmt.Printf("Successfully extended to %d!\n", circuitNodes[i].Data)
		}
	}
	fmt.Printf("Created circuit: %+v\n\n", circuitNodes)
}

// Creates a circuit between this router and the target router.
// Returns {0, 0} on failure.
func createCircuit(targetRouterID uint32, address string) circuit {
	for !openConnectionIfNotExists(address, targetRouterID) {
		return circuit{0, 0}
	}
	targetConnection := currentConnectionsRead(targetRouterID)
	initiatedConnectionLock.RLock()
	_, odd := initiatedConnection[targetRouterID]
	initiatedConnectionLock.RUnlock()
	newCircuitID := uint16(2 * rand.Intn(100000))
	if odd {
		newCircuitID = newCircuitID + 1
	}

	// In the rare case that there is already a circuit between this router
	// and the target with the random number that was just generated, retry.
	if _, ok := circuitToInputRead(circuit{newCircuitID, targetRouterID}); ok {
		newCircuitID = uint16(2 * rand.Intn(100000))
		if odd {
			newCircuitID = newCircuitID + 1
		}
	}

	cell := createCell(newCircuitID, create)
	result := circuit{newCircuitID, targetRouterID}
	if _, ok := circuitToInputRead(result); !ok {
		circuitToInputWrite(result, make(chan []byte, bufferSize))
	}
	targetConnection <- cell
	inputs, _ := circuitToInputRead(result)
	reply := <-inputs
	_, replyType := cellCIDAndType(reply)
	if replyType != created {
		circuitToInputLock.Lock()
		delete(circuitToInput, result)
		circuitToInputLock.Unlock()
		return circuit{0, 0}
	}
	return result
}

func openConnectionIfNotExists(address string, targetRouterID uint32) bool {
	_, ok := currentConnections.Get(targetRouterID)
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
	reply := make([]byte, 512)
	_, err = conn.Read(reply)
	if err != nil {
		return false
	}
	_, replyType := cellCIDAndType(reply)
	if replyType != opened {
		return false
	}
	currentConnections.SetIfAbsent(targetRouterID, make(chan []byte, sendBufferSize))
	initiatedConnectionLock.Lock()
	initiatedConnection[targetRouterID] = true
	initiatedConnectionLock.Unlock()
	go handleConnection(conn, targetRouterID)
	return true
}

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
			conn.Close()
			return
		} else {
			// Opened connection.
			cell[2] = opened
			conn.Write(cell)
		}
	} else {
		cell[2] = openFailed
		conn.Write(cell)
		conn.Close()
		return
	}
	currentConnections.SetIfAbsent(targetRouterID, make(chan []byte, sendBufferSize))
	handleConnection(conn, targetRouterID)
}

func handleConnection(conn net.Conn, targetRouterID uint32) {
	toSend := currentConnectionsRead(targetRouterID)
	// Thread that blocks on data to send and sends it.
	go func() {
		for {
			cell, stillOpen := <-toSend
			if !stillOpen {
				return
			}
			_, err := conn.Write(cell)
			if err != nil {
				return
			}
		}
	}()
	c := bufio.NewReader(conn)
	for {
		// Wait for data to come on the channel then process it.
		cell := make([]byte, 512)
		_, err := io.ReadFull(c, cell)
		if err != nil {
			fmt.Printf("Connection failure from %d! Deleting router information...\n", targetRouterID)
			deleteRouter(targetRouterID)
			conn.Close()
			return
		}
		circuitID, cellType := cellCIDAndType(cell)
		switch cellType {
		case create:
			// Other agent wants to create a new circuit.
			// Need to ensure that this circuit ID is unique.
			// We are now the endpoint of some circuit.
			routingTableLock.RLock()
			_, ok := routingTableForward[circuit{circuitID, targetRouterID}]
			routingTableLock.RUnlock()
			if ok {
				// Circuit already existed.
				cell[2] = createFailed
			} else {
				// Otherwise, create a channel to put that circuits data on
				// and add it to the routing table.
				_, ok := circuitToInputRead(circuit{circuitID, targetRouterID})
				circuitToIsEndWrite(circuit{circuitID, targetRouterID}, true)
				cell[2] = created
				if !ok {
					// Make sure it doesn't already exist.
					circuitToInputWrite(circuit{circuitID, targetRouterID},
						make(chan []byte, bufferSize))
					// If not, someone is already watching the channel (self)
					go watchChannel(circuit{circuitID, targetRouterID})
				}
			}
			fmt.Printf("Created circuit %d to %d\n", circuitID, targetRouterID)
			toSend <- cell
			continue
		default:
			// Send the data directly to the circuit handler.
			if cellType > 8 || cellType < 0 {
				continue
			}
			inputs, _ := circuitToInputRead(circuit{circuitID, targetRouterID})
			if inputs == nil {
				continue
			}
			inputs <- cell
		}
	}
}

// Handles all messages sent by agent A on circuit C.
func watchChannel(c circuit) {
	circuitToReplyLock.RLock()
	_, ok := circuitToReply[c]
	circuitToReplyLock.RUnlock()
	if !ok {
		circuitToReplyLock.Lock()
		circuitToReply[c] = make(chan []byte, bufferSize)
		circuitToReplyLock.Unlock()
	}
	for {
		inputs, _ := circuitToInputRead(c)
		cell := <-inputs
		_, cellType := cellCIDAndType(cell)
		_, endOfRelay := circuitToIsEndRead(c)
		// The circuitID should already be known.
		switch cellType {
		case relayCell:
			r := parseRelay(cell)
			if r.body == nil {
				continue
			}
			switch r.relayCommand {
			case extend:
				extendRelay(r, c, endOfRelay, cell)
			case begin:
				beginRelay(r, c, endOfRelay, cell)
			case end:
				endStream(r, c, endOfRelay, cell)
			case data:
				handleData(r, c, cell)
			case connected:
				handleStreamMessages(r, c, cell)
			case beginFailed:
				handleStreamMessages(r, c, cell)
			case extended:
				handleCircuitMessages(r, c, cell)
			case extendFailed:
				handleCircuitMessages(r, c, cell)
			}
		case destroy:
			if !routeCellBackwards(c, cell) {
				routeCellForwards(c, cell)
			}
			destroyCircuit(c)
			if (firstCircuit == circuit{0, 0}) {
				go createInitialCircuit()
			}
		}
	}
}

func destroyCircuit(c circuit) {
	circuitToReplyLock.Lock()
	circuitToIsEndLock.Lock()
	circuitToInputLock.Lock()
	routingTableLock.Lock()
	if firstCircuit == c {
		firstCircuit = circuit{0, 0}
	}
	delete(circuitToReply, c)
	delete(circuitToIsEnd, c)
	delete(circuitToInput, c)
	otherEnd, ok := routingTableForward[c]
	if ok {
		delete(routingTableForward, c)
		delete(routingTableBackward, otherEnd)
	} else {
		otherEnd, ok = routingTableBackward[c]
		if ok {
			delete(routingTableForward, otherEnd)
			delete(routingTableBackward, c)
		}
	}
	routingTableLock.Unlock()
	circuitToInputLock.Unlock()
	circuitToIsEndLock.Unlock()
	circuitToReplyLock.Unlock()
	return
}

func handleCircuitMessages(r relay, c circuit, cell []byte) {
	// Extended, extend failed.
	if !routeCellBackwards(c, cell) {
		circuitToReplyLock.RLock()
		channel := circuitToReply[c]
		if channel == nil {
			return
		}
		circuitToReplyLock.RUnlock()
		channel <- cell
	}
}

func handleStreamMessages(r relay, c circuit, cell []byte) {
	// Connected, begin failed.
	if !routeCellBackwards(c, cell) {
		channel := streamToReceiverRead(r.streamID)
		if channel == nil {
			return
		}
		channel <- cell
	}
}

func routeCellBackwards(c circuit, cell []byte) bool {
	routingTableLock.RLock()
	previousCircuit, back := routingTableBackward[c]
	routingTableLock.RUnlock()
	if back {
		binary.BigEndian.PutUint16(cell[:2], previousCircuit.circuitID)
		sendCellToAgent(previousCircuit.routerID, cell)
		return true
	}
	return false
}

func routeCellForwards(c circuit, cell []byte) bool {
	routingTableLock.RLock()
	nextCircuit, front := routingTableForward[c]
	routingTableLock.RUnlock()
	if front {
		binary.BigEndian.PutUint16(cell[:2], nextCircuit.circuitID)
		sendCellToAgent(nextCircuit.routerID, cell)
		return true
	}
	return false
}

// r is the relay sent on circuit c.
// endOfRelay indicates whether this router is the last stop in the relay.
func extendRelay(r relay, c circuit, endOfRelay bool, cell []byte) {
	if endOfRelay {
		address := string(r.body[:clen(r.body)])
		targetRouterID := binary.BigEndian.Uint32(r.body[(clen(r.body))+1:])
		if targetRouterID == routerID {
			// Don't extend a relay to ourselves.
			sendCellToAgent(c.routerID, createRelay(r.circuitID, r.streamID,
				r.digest, 0, extended, nil))
			return
		}
		result := createCircuit(targetRouterID, address)
		if (result != circuit{0, 0}) {
			routingTableLock.Lock()
			routingTableForward[c] = result
			routingTableBackward[result] = c
			routingTableLock.Unlock()
			circuitToIsEndLock.Lock()
			delete(circuitToIsEnd, c)
			circuitToIsEndLock.Unlock()
			sendCellToAgent(c.routerID, createRelay(r.circuitID, r.streamID,
				r.digest, 0, extended, nil))
			go watchChannel(result)
		} else {
			sendCellToAgent(c.routerID, createRelay(r.circuitID, r.streamID,
				r.digest, 0, extendFailed, nil))
		}
	} else {
		// If we're not the end of the relay, just foward the message.
		routeCellForwards(c, cell)
	}
}

func handleData(r relay, c circuit, cell []byte) {
	if routeCellBackwards(c, cell) {
		return
	}
	if routeCellForwards(c, cell) {
		return
	}
	// This must be the endpoint, since there's nowhere to route it.
	channel := streamToReceiverRead(r.streamID)
	if channel == nil {
		return
	}
	channel <- cell
}

func sendCellToAgent(targetRouterID uint32, cell []byte) {
	if channel, ok := currentConnections.Get(targetRouterID); ok {
		channel.(chan []byte) <- cell
	}
}

func endStream(r relay, c circuit, endOfRelay bool, cell []byte) {
	if routeCellBackwards(c, cell) {
		return
	}
	if routeCellForwards(c, cell) {
		return
	}
	// This must be the endpoint, since there's nowhere to route it.
	channel := streamToReceiverRead(r.streamID)
	if channel == nil {
		return
	}
	close(channel)
	streamToReceiver.Remove(r.streamID)
	return
}

func beginRelay(r relay, c circuit, endOfRelay bool, cell []byte) {
	if !endOfRelay {
		routeCellForwards(c, cell)
		return
	}
	address := string(r.body[:clen(r.body)])
	conn, err := net.Dial("tcp", address)
	if err != nil {
		sendCellToAgent(c.routerID, createRelay(r.circuitID, r.streamID,
			r.digest, 0, beginFailed, nil))
		return
	}

	if _, ok := streamToReceiver.Get(r.streamID); ok {
		// Stream ID is not unique.
		sendCellToAgent(c.routerID, createRelay(r.circuitID, r.streamID,
			r.digest, 0, beginFailed, nil))
		return
	}
	streamToReceiver.Set(r.streamID, make(chan []byte, bufferSize))
	fmt.Printf("Created stream %d to %s\n", r.streamID, address)
	go handleStreamEnd(conn, r.streamID, c)
	sendCellToAgent(c.routerID, createRelay(r.circuitID, r.streamID,
		r.digest, 0, connected, nil))
	circuitToIsEndWrite(c, true)
}

// The server end of the relay.
func handleStreamEnd(conn net.Conn, streamID uint16, c circuit) {
	channel := streamToReceiverRead(streamID)
	if channel == nil {
		return
	}
	cell, alive := <-channel
	if !alive {
		return
	}
	relayReply := parseRelay(cell)
	if relayReply.relayCommand == end {
		conn.Close()
		return
	} else if relayReply.relayCommand == data {
		conn.Write(relayReply.body)
		go func() {
			channel := streamToReceiverRead(streamID)
			if channel == nil {
				// Channel must have been closed.
				conn.Close()
				return
			}
			for {
				// In HTTP, in case there is more to a request.
				cell, alive := <-channel
				if !alive {
					conn.Close()
					return
				}
				relayReply = parseRelay(cell)
				if relayReply.relayCommand == data {
					_, err := conn.Write(relayReply.body)
					if err != nil {
						// Connection must have been closed here.
						// We now need to wait for the other end to
						// close their side.
						break
					}
				}
			}
			for {
				_, alive := <-channel
				if !alive {
					return
				}
			}
		}()
		for {
			buffer := make([]byte, maxDataSize)
			n, err := conn.Read(buffer)
			if err != nil {
				toSend := createRelay(c.circuitID, streamID, 0, 0, end, nil)
				sendCellToAgent(c.routerID, toSend)
				// We don't explicitly close the channel until we receive an
				// end message. Otherwise, we risk the chance of closing while
				// someone sends data.
				streamToReceiver.Remove(streamID)
				conn.Close()
				return
			}
			sendData(c, streamID, 0, buffer[:n])
		}
	}
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

func sendData(c circuit, streamID uint16, digest uint32, body []byte) {
	relay := createRelay(c.circuitID, streamID, digest, uint16(len(body)), data, body)
	sendCellToAgent(c.routerID, relay)
}

// DATA MANIPULATION FUNCTIONS

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

func cellCIDAndType(cell []byte) (uint16, uint8) {
	return binary.BigEndian.Uint16(cell[:2]), cell[2]
}

func parseRelay(cell []byte) relay {
	circuitID, _ := cellCIDAndType(cell)
	streamID := binary.BigEndian.Uint16(cell[3:5])
	digest := binary.BigEndian.Uint32(cell[7:11])
	bodyLength := binary.BigEndian.Uint16(cell[11:13])
	relayCommand := cell[13]
	if 14+bodyLength > 512 {
		return relay{0, 0, 1, 0, 0, nil}
	}
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

func createStream(streamID uint16, address string) bool {
	var body []byte
	body = append(body, address...)
	body = append(body, 0)
	streamToReceiver.Set(streamID, make(chan []byte, bufferSize))
	relay := createRelay(firstCircuit.circuitID, streamID, 0, uint16(len(body)),
		begin, body)
	sendCellToAgent(firstCircuit.routerID, relay)
	channel := streamToReceiverRead(streamID)
	reply, alive := <-channel
	if !alive {
		streamToReceiver.Remove(streamID)
		return false
	}
	relayReply := parseRelay(reply)
	if relayReply.relayCommand != connected {
		streamToReceiver.Remove(streamID)
		return false
	}
	return true
}

func proxyServer(port uint16) {
	fmt.Printf("Proxy listening on port %d\n", port)
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
	if (firstCircuit == circuit{0, 0}) {
		conn.Close()
		return
	}
	header := p.ParseHTTPRequest(conn)
	if header.IP == "" {
		conn.Close()
		return
	}
	ok := true
	streamID := uint16(0)
	for ok {
		streamID = uint16(rand.Intn(1000000))
		_, ok = streamToReceiver.Get(streamID)
	}
	if !createStream(streamID, header.IP+":"+header.Port) {
		fmt.Printf("Could not connect to %s.\n", header.IP+":"+header.Port)
		conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		conn.Close()
		return
	}
	fmt.Printf("Created stream %d to %s.\n", streamID, header.IP+":"+header.Port)
	if !header.HTTPS {
		// Now that we have a stream, we send over the initial request via a data
		// message.
		splitData := splitUpResult(header.Data, maxDataSize)
		for _, request := range splitData {
			// First, we send all the data...
			sendData(firstCircuit, streamID, 0, request)
		}
		// Shouldn't reread, actually, and the endstream shouldn't delete.
		channel := streamToReceiverRead(streamID)
		if channel == nil {
			// Channel must have been closed before we could even start.
			conn.Close()
			return
		}
		for {
			// Then, we read all the data (until the other end gets an EOF).
			cell, alive := <-channel
			if !alive {
				conn.Close()
				return
			}
			replyRelay := parseRelay(cell)
			if replyRelay.relayCommand == data {
				_, err := conn.Write(replyRelay.body)
				if err != nil {
					toSend := createRelay(firstCircuit.circuitID, streamID, 0, 0, end, nil)
					sendCellToAgent(firstCircuit.routerID, toSend)
					// This means that we may never hear from the endStream.
					streamToReceiver.Remove(streamID)
					break
				}
			}
		}
		conn.Close()
		for {
			_, alive := <-channel
			if !alive {
				return
			}
		}
	} else {
		conn.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
		go func() {
			channel := streamToReceiverRead(streamID)
			if channel == nil {
				// Channel must have been closed.
				conn.Close()
				return
			}
			for {
				// Reading data from the Tor network to the browser.
				cell, alive := <-channel
				if !alive {
					conn.Close()
					return
				}
				relayReply := parseRelay(cell)
				if relayReply.relayCommand == data {
					_, err := conn.Write(relayReply.body)
					if err != nil {
						break
					}
				}
			}
			for {
				_, alive := <-channel
				if !alive {
					return
				}
			}
		}()
		for {
			// Sending data from the browser to the Tor network.
			buffer := make([]byte, maxDataSize)
			n, err := conn.Read(buffer)
			if err != nil {
				toSend := createRelay(firstCircuit.circuitID, streamID, 0, 0, end, nil)
				sendCellToAgent(firstCircuit.routerID, toSend)
				streamToReceiver.Remove(streamID)
				conn.Close()
				return
			}
			sendData(firstCircuit, streamID, 0, buffer[:n])
		}
	}

}

func splitUpResult(result []byte, chunkSize int) [][]byte {
	var divided [][]byte

	for i := 0; i < len(result); i += chunkSize {
		end := i + chunkSize

		if end > len(result) {
			end = len(result)
		}

		divided = append(divided, result[i:end])
	}
	return divided
}

func clen(n []byte) int {
	for i := 0; i < len(n); i++ {
		if n[i] == 0 {
			return i
		}
	}
	return len(n)
}

func main() {
	if len(os.Args) != 5 {
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
	address := flag.Arg(3)
	routerName := "Tor61Router-" + fmt.Sprintf("%04d", group) + "-" + fmt.Sprintf("%04d", instance)
	routerNum := (group << 16) | (instance)
	fmt.Printf("Router name: %s\n", routerName)
	proxyPort = uint16(proxy)
	routerID = uint32(routerNum)
	fmt.Printf("Router ID: %d\n", routerID)

	fmt.Println(getOutboundIP())
	go func() {
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()
	StartRouter(routerName, group, address)

}

func usage() {
	fmt.Println("Usage: ./run <group number> <instance number> <HTTP Proxy port> <registration service address:registration service port>")
	os.Exit(2)
}

//function to get the public ip address
func getOutboundIP() string {
	conn, _ := net.Dial("udp", "8.8.8.8:80")
	defer conn.Close()
	localAddr := conn.LocalAddr().String()
	idx := strings.LastIndex(localAddr, ":")
	return localAddr[0:idx]
}

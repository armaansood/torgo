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
	mapuint16 "torgo/concurrent_uint16map"
	mapuint32 "torgo/concurrent_uint32map"
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

const bufferSize int = 10000000
const circuitLength int = 3
const maxDataSize int = 497

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
var currentConnections = mapuint32.New()

//var currentConnections2 = make(map[uint32](chan []byte))
//var currentConnectionsLock2 = sync.RWMutex{}

func currentConnectionsRead(routerID uint32) chan []byte {
	//currentConnectionsLock.RLock()
	//defer currentConnectionsLock.RUnlock()
	result, _ := currentConnections.Get(routerID)
	return result.(chan []byte)
}

// Maps from a circuit to a reading channel.
var circuitToInput = make(map[circuit](chan []byte))
var circuitToInputLock = sync.RWMutex{}

//var circuitToInput = mapcircuit.New()

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

// incorrect, can have multiple streams per circuit
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

//var streamToReceiver2 = make(map[uint16](chan []byte))

var streamToReceiver = mapuint16.New()

func streamToReceiverRead(streamID uint16) chan []byte {
	result, ok := streamToReceiver.Get(streamID)
	if !ok {
		fmt.Printf("Error guy %d\n", streamID)
		//	return nil
	}
	return result.(chan []byte)
}

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

func sliceUniqMap(s []int) []int {
	seen := make(map[int]struct{}, len(s))
	j := 0
	for _, v := range s {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		s[j] = v
		j++
	}
	return s[:j]
}

func deleteRouter(targetRouterID uint32) {
	for k, v := range routingTableForward {
		if k.agentID == targetRouterID || v.agentID == targetRouterID {
			//close(circuitToInput[k])
			//close(circui
			delete(routingTableForward, k)
			delete(routingTableBackward, v)
			circuitToInputLock.Lock()
			delete(circuitToInput, k)
			delete(circuitToInput, v)
			circuitToInputLock.Unlock()
			circuitToIsEndLock.Lock()
			delete(circuitToIsEnd, k)
			delete(circuitToIsEnd, v)
			circuitToIsEndLock.Unlock()
		}
		//		delete(currentConnections, targetRouterID)
		delete(initiatedConnection, targetRouterID)
	}
}

func StartRouter(routerName string, group string) {
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
		}

		fmt.Printf("Routing table forward: %+v, backward %+v\n", routingTableForward, routingTableBackward)
		//		fmt.Printf("Current connections: %+v\n", currentConnections)
		fmt.Printf("Initiated connection: %+v\n", initiatedConnection)
		fmt.Printf("First hop circuit: %+v\n", firstCircuit)
		//		fmt.Printf("Stream to data: %+v\n\n", streamToReceiver)
		cleanup()
		os.Exit(1)
	}()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println(err)
		os.Exit(2)
	}
	port = uint16(l.Addr().(*net.TCPAddr).Port)
	agent = new(r.Agent)
	agent.StartAgent(regServerIP, regServerPort, false)
	for !(agent.Register("127.0.0.1", port, routerID, uint8(len(routerName)), routerName)) {
		fmt.Println("Could not register self! Retrying...")
	}
	fmt.Printf("Registered router %d on port %d\n", routerID, port)
	rand.Seed(time.Now().Unix())
	var responses []r.FetchResponse
	go routerServer(l)
	fmt.Println("Fetching other routers...")
	responses = agent.Fetch("Tor61Router-" + group)
	for len(responses) < 2 {
		fmt.Println("No other routers online. Waiting 5 seconds and retrying...")
		time.Sleep(3 * time.Second)
		responses = agent.Fetch("Tor61Router-" + group)
		fmt.Println(len(responses))
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
		firstCircuit = createCircuit(circuitNodes[0].Data,
			circuitNodes[0].IP+":"+strconv.Itoa(int(circuitNodes[0].Port)))
		// If this node failed, try another.
		if (firstCircuit == circuit{0, 0}) {
			fmt.Printf("Failed to connect to %d\n", circuitNodes[0].Data)
			circuitNodes[0] = responses[rand.Intn(len(responses))]
			fmt.Printf("Retrying with router %d\n", circuitNodes[0].Data)
		}
	}
	//fmt.Println("Getting a lock")
	//	currentConnectionsLock.RLock()
	firstHopConnection := currentConnectionsRead(firstCircuit.agentID)
	//	currentConnectionsLock.RUnlock()
	//	fmt.Println("UNLOCKING!")
	circuitToReplyLock.Lock()
	circuitToReply[firstCircuit] = make(chan []byte, bufferSize)
	circuitToReplyLock.Unlock()
	go watchChannel(firstCircuit)
	for i := 1; i < circuitLength; i++ {
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
			fmt.Printf("Retrying with router %d\n", circuitNodes[i].Data)
			i--
		} else {
			fmt.Printf("Successfully extended to %d\n\n", circuitNodes[i].Data)
		}
		// fmt.Printf("Routing table forward: %+v, backward %+v\n", routingTableForward, routingTableBackward)
		// fmt.Printf("Current connections: %+v\n", currentConnections)
		// fmt.Printf("Initiated connection: %+v\n", initiatedConnection)
		// fmt.Printf("First hop circuit: %+v\n", firstCircuit)
		// fmt.Printf("Streams: %+v\n\n", circuitToStream)
	}
	proxyServer(proxyPort)

}

// Creates a circuit between this router and the target router.
// Returns {0, 0} on failure.
func createCircuit(targetRouterID uint32, address string) circuit {
	for !openConnectionIfNotExists(address, targetRouterID) {
		return circuit{0, 0}
	}
	//currentConnectionsLock.RLock()
	targetConnection := currentConnectionsRead(targetRouterID)
	//currentConnectionsLock.RUnlock()
	_, odd := initiatedConnection[targetRouterID]
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
		//		delete(circuitToInput, result)
		return circuit{0, 0}
	}
	return result
}

func openConnectionIfNotExists(address string, targetRouterID uint32) bool {
	//currentConnectionsLock.RLock()
	_, ok := currentConnections.Get(targetRouterID)
	//currentConnectionsLock.RUnlock()
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
	//currentConnectionsLock.RLock()
	currentConnections.SetIfAbsent(targetRouterID, make(chan []byte, bufferSize))
	// if _, ok := currentConnections[targetRouterID]; !ok {
	// 	//currentConnectionsLock.RUnlock()
	// 	//currentConnectionsLock.Lock()
	// 	currentConnections[targetRouterID] = make(chan []byte, bufferSize)
	// 	//currentConnectionsLock.Unlock()
	// } else {
	// 	//currentConnectionsLock.RUnlock()
	// }
	initiatedConnection[targetRouterID] = true
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
	// If it's a self-connection, we're already listening to ourselves.
	//currentConnectionsLock.RLock()
	currentConnections.SetIfAbsent(targetRouterID, make(chan []byte, 10000000))
	// if _, ok := currentConnections[targetRouterID]; !ok {
	// 	currentConnections[targetRouterID] = make(chan []byte, bufferSize)
	// }
	//currentConnectionsLock.RUnlock()
	handleConnection(conn, targetRouterID)
}

func handleConnection(conn net.Conn, targetRouterID uint32) {
	//currentConnectionsLock.RLock()
	toSend := currentConnectionsRead(targetRouterID)
	//currentConnectionsLock.RUnlock()
	// Thread that blocks on data to send and sends it.
	go func() {
		for {

			cell, stillOpen := <-toSend
			if !stillOpen {
				fmt.Printf("deleting!!!! router %d\n", targetRouterID)
				return
			}
			// circuitID, mType := cellCIDAndType(cell)
			// fmt.Printf("%d: Sending message to %d of type %d on circuit %d\n", routerID, targetRouterID, mType, circuitID)
			// if mType == relayCell {
			// 	fmt.Printf("Relay of type: %d\n", parseRelay(cell).relayCommand)
			// }
			// fmt.Printf("Routing table forward: %+v, backward %+v\n", routingTableForward, routingTableBackward)
			// fmt.Printf("Current connections: %+v\n", currentConnections)
			// fmt.Printf("Initiated connection: %+v\n", initiatedConnection)
			// fmt.Printf("First hop circuit: %+v\n", firstCircuit)
			// fmt.Printf("Streams: %+v\n", circuitToStream)
			//			fmt.Printf("About to write...")
			n, err := conn.Write(cell)
			if len(toSend) > 10000000-2 {

				fmt.Printf("Wrote data, size %d, wwrote n %d\n", len(toSend), n)
			}
			//
			if err != nil {
				fmt.Println("ERROR")
				fmt.Println(err)
				return
			}
		}
	}()
	for {
		// Wait for data to come on the channel then process it.
		cell := make([]byte, 512)
		_, err := conn.Read(cell)
		if err != nil {
			fmt.Printf("Routing table forward: %+v, backward %+v\n", routingTableForward, routingTableBackward)
			//			fmt.Printf("Current connections: %+v\n", currentConnections)
			fmt.Printf("Initiated connection: %+v\n", initiatedConnection)
			fmt.Printf("First hop circuit: %+v\n", firstCircuit)
			//			fmt.Printf("Streams: %+v\n\n", circuitToStream)
			deleteRouter(targetRouterID)
			fmt.Println(err)
			fmt.Printf("Error on connection from %d\n", targetRouterID)
			conn.Close()
			//currentConnectionsLock.Lock()
			currentConnections.Remove(targetRouterID)
			//			delete(currentConnections, targetRouterID)
			//currentConnectionsLock.Unlock()
			return
		}
		circuitID, cellType := cellCIDAndType(cell)
		//	fmt.Printf("%d: Received message from %d on circuit %d of type %d\n", routerID, targetRouterID, circuitID, cellType)
		// if cellType == relayCell {
		// 	fmt.Printf("Relay of type: %d\n", parseRelay(cell).relayCommand)
		// 	fmt.Printf("Stream: %d\n", parseRelay(cell).streamID)
		// }
		switch cellType {
		case create:
			// Other agent wants to create a new circuit.
			// Need to ensure that this circuit ID is unique.
			// We are now the endpoint of some circuit.
			_, ok := routingTableForward[circuit{circuitID, targetRouterID}]
			if ok {
				// Circuit already existed.
				cell[2] = createFailed
			} else {
				// Otherwise, create a channel to put that circuits data on
				// and add it to the routing table.
				_, ok := circuitToInputRead(circuit{circuitID, targetRouterID})
				circuitToIsEndWrite(circuit{circuitID, targetRouterID}, true)
				cell[2] = created
				//	fmt.Println("Creating circuit")
				if !ok {
					// Make sure it doesn't already exist.
					circuitToInputWrite(circuit{circuitID, targetRouterID}, make(chan []byte, bufferSize))
					// If not, someone is already watching the channel (self)
					go watchChannel(circuit{circuitID, targetRouterID})
				}
			}
			toSend <- cell
			continue
		default:
			//	fmt.Printf("putting data on the circuit %+v\n", circuitToInput[circuit{circuitID, targetRouterID}])
			//fmt.Printf("Length of circuit to input: %d\n", len(circuitToInput[circuit{circuitID, targetRouterID}]))
			inputs, _ := circuitToInputRead(circuit{circuitID, targetRouterID})
			inputs <- cell
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
		if targetRouterID == routerID {
			// If we're trying to extend a relay to ourselves, don't!
			sendCellToAgent(c.agentID, createRelay(r.circuitID, r.streamID,
				r.digest, 0, extended, nil))
			return
		}
		result := createCircuit(targetRouterID, address)
		if (result != circuit{0, 0}) {
			routingTableForward[c] = result
			routingTableBackward[result] = c
			circuitToIsEndLock.Lock()
			delete(circuitToIsEnd, c)
			circuitToIsEndLock.Unlock()
			sendCellToAgent(c.agentID, createRelay(r.circuitID, r.streamID,
				r.digest, 0, extended, nil))
			go watchChannel(result)
		} else {
			sendCellToAgent(c.agentID, createRelay(r.circuitID, r.streamID,
				r.digest, 0, extendFailed, nil))
		}
	} else {
		// If we're not the end of the relay, just foward the message.
		endPoint := routingTableForward[c]
		binary.BigEndian.PutUint16(cell[:2], endPoint.circuitID)
		sendCellToAgent(endPoint.agentID, cell)
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
		//		fmt.Println(circuitToInput)
		inputs, _ := circuitToInputRead(c)
		cell := <-inputs
		_, cellType := cellCIDAndType(cell)
		_, endOfRelay := circuitToIsEndRead(c)
		// The circuitID should already be known.
		switch cellType {
		case relayCell:
			r := parseRelay(cell)
			//			fmt.Printf("data received on the cell %v, length %d, type %d\n", c, len(circuitToInput[c]), r.relayCommand)
			switch r.relayCommand {
			case extend:
				//	fmt.Println("extend")
				extendRelay(r, c, endOfRelay, cell)
			case begin:
				//	fmt.Println("begin")
				go beginRelay(r, c, endOfRelay, cell)
			//	fmt.Println("begin done")
			case end:
				//	fmt.Println("end")
				endStream(r, c, endOfRelay, cell)
			//	fmt.Println("end done")
			case data:
				//	fmt.Println("data")
				//		go func() {
				previousCircuit, back := routingTableBackward[c]
				nextCircuit, front := routingTableForward[c]
				if back {
					//	fmt.Println("Forwarding")
					//					fmt.Printf("Size of backward: %d\n", len(currentConnectionsRead(previousCircuit.agentID)))
					// If the circuit is travelling backwards.
					binary.BigEndian.PutUint16(cell[:2], previousCircuit.circuitID)
					//					fmt.Printf("backwarding size %d\n", len(currentConnectionsRead(previousCircuit.agentID))
					sendCellToAgent(previousCircuit.agentID, cell)
				} else if front {
					//					fmt.Println("forwardwarding")
					binary.BigEndian.PutUint16(cell[:2], nextCircuit.circuitID)
					//					fmt.Printf("fowrard size %d\n", len(currentConnectionsRead(nextCircuit.agentID)))
					sendCellToAgent(nextCircuit.agentID, cell)
				} else {
					// This must be the endpoint, since there's nowhere to route it.
					//					fmt.Printf("Stream to receiver size %d\n", len(streamToReceiver[r.streamID]))
					//					fmt.Println(streamToReceiver)
					//					fmt.Println(r.streamID)
					//					fmt.Printf("stream %d, size: %d\n", r.streamID, len(streamToReceiverRead(r.streamID)))
					//					fmt.Printf("Length of circuit to input: %d\n", len(circuitToInput[c]))
					channel := streamToReceiverRead(r.streamID)
					if channel == nil {
						//							fmt.Println(string(r.body))
						//
						//							fmt.Println("FAIL")
					}
					channel <- r.body
				}
			//	}()
			//	fmt.Println("data complete")
			case connected:
				// Connected, extended, begin failed, extend failed.
				previousCircuit, ok := routingTableBackward[c]
				if !ok {
					streamToReceiverRead(r.streamID) <- cell
				} else {
					binary.BigEndian.PutUint16(cell[:2], previousCircuit.circuitID)
					sendCellToAgent(previousCircuit.agentID, cell)
				}
			case beginFailed:
				// Connected, extended, begin failed, extend failed.
				previousCircuit, ok := routingTableBackward[c]
				if !ok {
					streamToReceiverRead(r.streamID) <- cell
				} else {
					binary.BigEndian.PutUint16(cell[:2], previousCircuit.circuitID)
					sendCellToAgent(previousCircuit.agentID, cell)
				}
			//	fmt.Println("connected done")
			default:
				// Connected, extended, begin failed, extend failed.
				previousCircuit, ok := routingTableBackward[c]
				if !ok {
					circuitToReplyLock.RLock()
					circuitToReply[c] <- cell
					circuitToReplyLock.RUnlock()
				} else {
					binary.BigEndian.PutUint16(cell[:2], previousCircuit.circuitID)
					sendCellToAgent(previousCircuit.agentID, cell)
				}
				//	fmt.Println("default done")
			}

			//			case destroy:
		}
	}
}

func sendCellToAgent(agentID uint32, cell []byte) {
	//currentConnectionsLock.RLock()
	if channel, ok := currentConnections.Get(agentID); ok {
		channel.(chan []byte) <- cell
	} else {
		//		fmt.Printf("Error: Agent %d has no channel\n", agentID)
	}
	//currentConnectionsLock.RUnlock()
}

func endStream(r relay, c circuit, endOfRelay bool, cell []byte) {
	previousCircuit, back := routingTableBackward[c]
	nextCircuit, front := routingTableForward[c]
	if back {
		// If the circuit is travelling backwards.
		binary.BigEndian.PutUint16(cell[:2], previousCircuit.circuitID)
		sendCellToAgent(previousCircuit.agentID, cell)
	} else if front {
		binary.BigEndian.PutUint16(cell[:2], nextCircuit.circuitID)
		sendCellToAgent(nextCircuit.agentID, cell)
	} else {
		val, ok := streamToReceiver.Get(r.streamID)
		if ok {
			close(val.(chan []byte))
		}
		fmt.Printf("Ended stream %d\n", r.streamID)
		//delete(streamToReceiver, r.streamID)
	}
}

func beginRelay(r relay, c circuit, endOfRelay bool, cell []byte) {
	if !endOfRelay {
		endPoint := routingTableForward[c]
		binary.BigEndian.PutUint16(cell[:2], endPoint.circuitID)
		sendCellToAgent(endPoint.agentID, cell)
		return
	}
	//	fmt.Println("beginning relay")
	address := string(r.body[:clen(r.body)])
	conn, err := net.Dial("tcp", address)
	if err != nil {
		fmt.Println(err)
		fmt.Println(address)
		sendCellToAgent(c.agentID, createRelay(r.circuitID, r.streamID,
			r.digest, 0, beginFailed, nil))
		return
	}

	if _, ok := streamToReceiver.Get(r.streamID); ok {
		// Stream ID is not unique.
		fmt.Println("Stream ID is not unique!!!")
		sendCellToAgent(c.agentID, createRelay(r.circuitID, r.streamID,
			r.digest, 0, beginFailed, nil))
		return
	}
	streamToReceiver.Set(r.streamID, make(chan []byte, bufferSize))
	fmt.Printf("Created stream %d to %s\n", r.streamID, address)
	go handleStreamEnd(conn, r.streamID, c)
	sendCellToAgent(c.agentID, createRelay(r.circuitID, r.streamID,
		r.digest, 0, connected, nil))
	circuitToIsEndWrite(c, true)
}

// The server end of the relay.
func handleStreamEnd(conn net.Conn, streamID uint16, c circuit) {
	go func() {
		defer conn.Close()
		for {
			request, alive := <-streamToReceiverRead(streamID)
			if !alive {
				streamToReceiver.Remove(streamID)
				//	delete(streamToReceiver, streamID)
				return
			}
			_, err := conn.Write(request)
			if err != nil {
				fmt.Println("server error: ")
				fmt.Println(err)
			}
		}
	}()
	for {
		buffer := make([]byte, maxDataSize)
		n, err := conn.Read(buffer)
		if err != nil {
			fmt.Printf("Stream %d\tserver has no more data.\n", streamID)
			toSend := createRelay(c.circuitID, streamID, 0, 0, end, nil)
			sendCellToAgent(c.agentID, toSend)
			close(streamToReceiverRead(streamID))
			//			streamToReceiver.Remove(streamID)
			//			delete(streamToReceiver, streamID)
			return
		}
		sendData(c, streamID, 0, buffer[:n])
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
	sendCellToAgent(c.agentID, relay)
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
	//	fmt.Printf("Slice bound: %d\n", 14+bodyLength)
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
	sendCellToAgent(firstCircuit.agentID, relay)
	//	fmt.Printf("Waiting for a reply on circuit %+v\n", firstCircuit)
	// This won't work, there can be multiple streams created at once.
	//	fmt.Printf("received  reply\n")

	fmt.Printf("Waiting on stream %d\n", streamID)
	reply := <-streamToReceiverRead(streamID)
	relayReply := parseRelay(reply)
	if relayReply.relayCommand != connected {
		streamToReceiver.Remove(streamID)
		return false
	}
	fmt.Printf("Stream %d\tConnected to %s\n", streamID, address)
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
	header := p.ParseHTTPRequest(conn)
	if header.HTTPS {
		conn.Close()
		return
	}
	// need to make sure this is a unique stream.
	ok := true
	streamID := uint16(0)
	for ok {
		streamID = uint16(rand.Intn(1000000))
		_, ok = streamToReceiver.Get(streamID)
	}
	fmt.Println("Hoping to create stream %d to %s\n", streamID, header.IP+":"+header.Port)
	if !createStream(streamID, header.IP+":"+header.Port) {
		fmt.Printf("Could not connect to %s.\n", header.IP+":"+header.Port)
		conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		conn.Close()
		return
	}
	fmt.Println("Created stream %d to %s\n", streamID, header.IP+":"+header.Port)
	//	streamToReceiver[streamID] = make(chan []byte, bufferSize)
	if !header.HTTPS {
		// Now that we have a stream, we send over the initial request via a data
		// message.
		splitData := splitUpResult(header.Data, maxDataSize)
		for _, request := range splitData {
			sendData(firstCircuit, streamID, 0, request)
		}
		for {
			reply, alive := <-streamToReceiverRead(streamID)
			if !alive {
				streamToReceiver.Remove(streamID)
				//				delete(streamToReceiver, streamID)
				conn.Close()
				return
			}
			conn.Write(reply)
		}
	} else {
		conn.Close()
		return
		// conn.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
		// go func() {
		// 	for {
		// 		reply, alive := <-streamToReceiverRead(streamID)
		// 		if !alive {
		// 			//					streamToReceiver.Remove(streamID)
		// 			//					delete(streamToReceiver, streamID)
		// 			conn.Close()
		// 			return
		// 		}
		// 		conn.Write(reply)
		// 	}
		// }()
		// for {
		// 	buffer := make([]byte, maxDataSize)
		// 	n, err := conn.Read(buffer)
		// 	if err != nil {
		// 		fmt.Printf("Stream %d\tbrowser has no more data.\n", streamID)
		// 		//	toSend := createRelay(firstCircuit.circuitID, streamID, 0, 0, end, nil)
		// 		//	currentConnections[firstCircuit.agentID] <- toSend
		// 		//	close(streamToReceiver[streamID])
		// 		//				delete(streamToReceiver, streamID)
		// 		//				conn.Close()
		// 		return
		// 	}
		// 	reply := createRelay(firstCircuit.circuitID, streamID, 0, uint16(n), data, buffer[:n])
		// 	sendCellToAgent(firstCircuit.agentID, reply)
		// }
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
	proxyPort = uint16(proxy)
	routerID = uint32(routerNum)
	StartRouter(routerName, flag.Arg(0))
}

func usage() {
	fmt.Println("Usage: ./run <group number> <instance number> <HTTP Proxy port>")
	os.Exit(2)
}

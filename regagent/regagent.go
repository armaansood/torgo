package regagent

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

const (
	register      uint8  = 1
	registered    uint8  = 2
	fetch         uint8  = 3
	fetchresponse uint8  = 4
	unregister    uint8  = 5
	probe         uint8  = 6
	ack           uint8  = 7
	magicnumber   uint16 = 50273
)

// Agent represents the registration agent.
type Agent struct {
	sequenceNumber      uint8
	regServerIP         string
	regServerPort       string
	conn                net.Conn
	replies             chan []byte
	registeredAddresses map[string](bool)
	wg                  sync.WaitGroup
	debug               bool
}

type FetchResponse struct {
	IP   string
	Port uint16
	Data uint32
}

// StartAgent must be called before any other methods on the agent.
func (a *Agent) StartAgent(regServerIP string, regServerPort string, debug bool) {
	a.regServerIP = regServerIP
	a.regServerPort = regServerPort
	a.debug = debug
	a.replies = make(chan []byte)
	a.registeredAddresses = make(map[string](bool))
	var address bytes.Buffer
	address.WriteString(regServerIP)
	address.WriteString(":")
	address.WriteString(regServerPort)
	conn, err := net.Dial("udp", address.String())
	if err != nil {
		log.Fatal(err)
	}
	a.conn = conn
	pplusone := conn.LocalAddr().(*net.UDPAddr).Port + 1
	a.wg.Add(2)
	go a.regServerListener(pplusone, conn.LocalAddr().(*net.UDPAddr).IP.String())
	go a.listenForReplies()
}

func (a *Agent) listenForReplies() {
	defer a.wg.Done()
	for {
		data := make([]byte, 1024)
		_, err := a.conn.Read(data)
		if err != nil {
			fmt.Println(err)
			fmt.Println("issue with reading")
			continue
		}
		_, _, err = getDataType(data)
		if err != nil {
			continue
		}
		a.replies <- data
	}
}

// Retries 3 times and returns the response, or nil if none.
func (a *Agent) sendMessage(message []byte, expectedReply uint8, messageType string) []byte {
	i := 3
	timer := time.NewTimer(4 * time.Second)
	for i > 0 {
		_, err := a.conn.Write(message)
		if err != nil {
			fmt.Println(err)
			fmt.Println("issue with writing")
			continue
		}
		fmt.Println("Wrote")
		fmt.Println(message)
		select {
		case packet := <-a.replies:
			packetSequenceNumber, command, err := getDataType(packet)
			if err != nil || command != expectedReply || packetSequenceNumber != a.sequenceNumber {
				continue
			}
			timer.Stop()
			a.sequenceNumber++
			return packet
		case <-timer.C:
			if a.debug {
				fmt.Printf("Timed out waiting for reply to %s message\n", messageType)
			}
			i--
			timer.Reset(4 * time.Second)
		}
	}
	return nil
}

// Fetch requests that the server send back some,
// possibly all, existing registrations.
// Any registrations returned must themselves have service
// names that have the service name specified in this Fetch
// message as a prefix.
func (a *Agent) Fetch(serviceName string) []FetchResponse {
	header := createHeader(a.sequenceNumber, fetch)
	header = append(header, uint8(len(serviceName)))
	header = append(header, serviceName...)
	reply := a.sendMessage(header, fetchresponse, "FETCH")
	var responses []FetchResponse
	if a.debug && reply == nil {
		fmt.Println("Sent 3 FETCH messages but got no reply.")
	} else if reply != nil {
		numEntries := uint8(reply[4])
		for i := uint8(0); i < numEntries; i++ {
			serviceIP := binary.BigEndian.Uint32(reply[5+i*10 : 9+i*10])
			servicePort := binary.BigEndian.Uint16(reply[9+i*10 : 11+i*10])
			serviceData := binary.BigEndian.Uint32(reply[11+i*10 : 15+i*10])
			ipstring := fmt.Sprintf("%d.%d.%d.%d", byte(serviceIP>>24),
				byte(serviceIP>>16), byte(serviceIP>>8), byte(serviceIP))
			response := FetchResponse{IP: ipstring, Port: servicePort, Data: serviceData}
			responses = append(responses, response)
		}
	}
	return responses
}

// Probe tests if the server is still running.
func (a *Agent) Probe() {
	header := createHeader(a.sequenceNumber, probe)
	reply := a.sendMessage(header, ack, "PROBE")
	if a.debug {
		if reply != nil {
			fmt.Println("Probe successful.")
		} else {
			fmt.Println("Sent 3 PROBE messages but got no reply.")
		}
	}
}

// Unregister a given service IP and port.
func (a *Agent) Unregister(serviceIP string, servicePort uint16) {
	var sip uint32
	binary.Read(bytes.NewBuffer(net.ParseIP(serviceIP).To4()), binary.BigEndian, &sip)
	header := createHeader(a.sequenceNumber, unregister)
	data := make([]byte, 10)
	copy(data[:4], header[:])
	binary.BigEndian.PutUint32(data[4:8], sip)
	binary.BigEndian.PutUint16(data[8:10], servicePort)
	reply := a.sendMessage(data, ack, "UNREGISTER")
	if reply != nil {
		addr := net.UDPAddr{
			Port: int(servicePort),
			IP:   net.ParseIP(serviceIP),
		}
		delete(a.registeredAddresses, addr.String())
	} else if a.debug {
		fmt.Println("Sent 3 UNREGISTER messages but got no reply.")
	}
}

// Register a given service IP, port, data, and name.
func (a *Agent) Register(serviceIP string, servicePort uint16,
	serviceData uint32, len uint8, serviceName string) bool {
	addr := net.UDPAddr{
		Port: int(servicePort),
		IP:   net.ParseIP(serviceIP),
	}
	_, ok := a.registeredAddresses[addr.String()]
	if ok {
		fmt.Println("Registration failed: must unregister data under same address.")
		return false
	}
	a.registeredAddresses[addr.String()] = true
	if !a.performRegistration(serviceIP, servicePort, serviceData, len, serviceName, false) {
		delete(a.registeredAddresses, addr.String())
		return false
	}
	return true
}

func (a *Agent) performRegistration(serviceIP string, servicePort uint16,
	serviceData uint32, len uint8, serviceName string, automatic bool) bool {
	var sip uint32
	binary.Read(bytes.NewBuffer(net.ParseIP(serviceIP).To4()), binary.BigEndian, &sip)
	addr := net.UDPAddr{
		Port: int(servicePort),
		IP:   net.ParseIP(serviceIP),
	}
	_, ok := a.registeredAddresses[addr.String()]
	if !ok {
		return false
	}
	header := createHeader(a.sequenceNumber, register)
	data := make([]byte, 15)
	copy(data[:4], header[:])
	binary.BigEndian.PutUint32(data[4:8], sip)
	binary.BigEndian.PutUint16(data[8:10], servicePort)
	binary.BigEndian.PutUint32(data[10:14], serviceData)
	data[14] = len
	data = append(data, serviceName...)
	reply := a.sendMessage(data, registered, "REGISTER")
	if reply != nil {
		ttl := int(binary.BigEndian.Uint16(data[4:6]))
		if a.debug && !automatic {
			fmt.Printf("Register %s:%d successful: lifetime %d\n", serviceIP, servicePort, ttl)
		}
		f := func() {
			a.performRegistration(serviceIP, servicePort, serviceData, len, serviceName, true)
		}
		_ = time.AfterFunc(time.Duration(ttl/2)*time.Second, f)
		return true
	} else if a.debug {
		fmt.Println("Sent 3 REGISTER messages but got no reply.")
	}
	return false
}

func (a *Agent) regServerListener(port int, ip string) {
	defer a.wg.Done()
	addr := net.UDPAddr{
		Port: port,
		IP:   net.ParseIP(ip),
	}
	conn, err := net.ListenUDP("udp", &addr)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	for {
		data := make([]byte, 1024)
		_, remoteaddr, err := conn.ReadFromUDP(data)
		if err != nil {
			continue
		}
		sequenceNumber, command, err := getDataType(data)
		if err != nil || command != probe {
			continue
		}
		if a.debug {
			fmt.Println("I've been probed!")
		}
		ack := createHeader(sequenceNumber, ack)
		_, err = conn.WriteToUDP(ack, remoteaddr)
		if err != nil {
			continue
		}
	}
}

// Returns -1 if data is corrupt.
// Otherwise, returns the sequence number and the command type.
func getDataType(data []byte) (uint8, uint8, error) {
	magicNumber := binary.BigEndian.Uint16(data[:2])
	sequenceNumber := data[2]
	command := data[3]
	if magicNumber != magicnumber || command > 7 || command < 1 {
		return 0, 0, errors.New("Corrupt data")
	}
	return sequenceNumber, command, nil
}

func createHeader(sequenceNumber uint8, command uint8) []byte {
	header := make([]byte, 4)
	binary.BigEndian.PutUint16(header[:2], magicnumber)
	header[2] = sequenceNumber
	header[3] = command
	return header
}

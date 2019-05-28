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

func StartRouter(routerName string, agentID uint32, port uint16) {
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

func handleConnection(c net.Conn) {

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

func cellTypeAndCID(cell []byte) (uint16, uint8) {
	return binary.BigEndian.Uint16(data[:2]), data[3]
}

func main() {
	StartRouter("Tor61Router-4215-0001", 1238, 4611)
}

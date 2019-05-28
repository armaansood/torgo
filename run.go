package main

import (
	"bufio"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	m "torgo/regagent"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Println("Usage: ./run [hostname] [port]")
		os.Exit(2)
	}
	flag.Parse()
	hostname := flag.Arg(0)
	port := flag.Arg(1)
	localIP := GetOutboundIP()
	a := new(m.Agent)
	a.StartAgent(hostname, port, true)
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Printf("Enter r(egister), u(nregister), f(etch), p(robe), or q(uit): ")
		text, err := reader.ReadString('\n')
		if err != nil {
			os.Exit(2)
		}
		text = strings.TrimSuffix(text, "\n")
		text = strings.TrimSpace(text)
		args := strings.Fields(text)
		switch args[0] {
		case "q":
			os.Exit(2)
		case "r":
			if len(args) != 4 {
				fmt.Println("Invalid command!")
				continue
			}
			port, err := strconv.Atoi(args[1])
			if err != nil {
				fmt.Println("Invalid command!")
				continue
			}
			data, err := strconv.Atoi(args[2])
			if err != nil {
				fmt.Println("Invalid command!")
				continue
			}
			a.Register(localIP, uint16(port), uint32(data), uint8(len(args[3])), args[3])
		case "p":
			a.Probe()
		case "u":
			if len(args) != 2 {
				fmt.Println("Invalid command!")
				continue
			}
			port, err := strconv.Atoi(args[1])
			if err != nil {
				fmt.Println("Invalid command!")
				continue
			}
			a.Unregister(localIP, uint16(port))
		case "f":

			if len(args) > 2 {
				fmt.Println("Invalid command!")
				continue
			}
			var responses []m.FetchResponse
			if len(args) == 1 {
				responses = a.Fetch("")
			} else {
				responses = a.Fetch(args[1])
			}
			for index, element := range responses {
				fmt.Printf("[%d] %s %d %d\n", index+1, element.IP, element.Port, element.Data)
			}
		default:
			fmt.Println("Invalid command!")
		}
	}
}

// Function to get the public ip address.
func GetOutboundIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, address := range addrs {
		if ipnet, ok := address.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return ""
}

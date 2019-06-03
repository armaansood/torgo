package proxy

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
)

type HTTPRequest struct {
	IP    string
	Port  string
	Data  []byte
	HTTPS bool
}

func handleTunnel(client net.Conn, ip string, port string) {
	server, err := net.Dial("tcp", ip+":"+port)
	if err != nil {
		client.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		client.Close()
		return
	} else {
		client.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
	}
	go func() {
		buffer := make([]byte, 512)
		for {
			n, err := client.Read(buffer)
			if err != nil {
				client.Close()
				return
			}
			server.Write(buffer[:n])
		}
	}()
	go func() {
		buffer := make([]byte, 512)
		for {
			n, err := server.Read(buffer)
			if err != nil {
				server.Close()
				return
			}
			client.Write(buffer[:n])
		}
	}()
}

func ParseHTTPRequest(c net.Conn) HTTPRequest {
	reader := bufio.NewReader(c)
	var buffer bytes.Buffer
	port := ""
	ip := ""
	tunnel := false
	for {
		data, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if data == "\r\n" {
			buffer.WriteString(data)
			break
		}
		if buffer.Len() == 0 {
			firstLine := strings.Fields(data)
			if len(firstLine) > 2 {
				data = firstLine[0] + " " + firstLine[1] + " HTTP/1.0\r\n"
				lastColon := strings.LastIndex(firstLine[1], ":")
				port = strings.TrimSpace(firstLine[1][lastColon+1:])
				if _, err := strconv.Atoi(port); err != nil {
					if strings.HasPrefix(firstLine[1], "https://") {
						port = "443"
					} else {
						port = "80"
					}
				}
				fmt.Print(">>> " + data)
			}
			if strings.ToLower(strings.TrimSpace(firstLine[0])) == "connect" {
				tunnel = true
			}
		} else {
			colonIndex := strings.IndexByte(data, ':')
			if colonIndex > 0 {
				header, content := strings.ToLower(
					strings.TrimSpace(data[:colonIndex])),
					strings.ToLower(
						strings.TrimSpace(data[colonIndex+1:]))
				switch header {
				case "connection":
					data = "Connection: close\r\n"
				case "proxy-connection":
					data = "Proxy-connection: close\r\n"
				case "host":
					lastColon := strings.LastIndex(content, ":")
					potentialPort := strings.TrimSpace(content[lastColon+1:])
					if _, err := strconv.Atoi(potentialPort); err == nil {
						port = potentialPort
						ip = content[:lastColon]
					} else {
						ip = content
					}
				}
			}
		}
		buffer.WriteString(data)
	}
	return HTTPRequest{ip, port, buffer.Bytes(), tunnel}
}

func sendRequest(data []byte, ip string, port string) []byte {
	conn, err := net.Dial("tcp", ip+":"+port)
	if err != nil {
		return []byte("HTTP/1.1 502 Bad Gateway\r\n\r\n")
	}
	defer conn.Close()
	conn.Write(data)
	var buffer bytes.Buffer
	io.Copy(&buffer, conn)
	return buffer.Bytes()
}

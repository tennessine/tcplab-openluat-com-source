package main

import (
	"errors"
	"flag"
	"fmt"
	"github.com/gorilla/websocket"
	"log"
	"math/rand"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	PortMin = 10000
	PortMax = 55535

	FunctionCreateServer        = 0
	FunctionClientConnected     = 1
	FunctionClientDisconnected  = 2
	FunctionClientNormalMsg     = 3
	FunctionKickClient          = 4
	FunctionWsNormalMsg         = 5
	FunctionNotification        = 6
	FunctionServerIdleTimeLimit = 7

	ErrorOK          = 0
	ErrorIpUsed      = 1
	ErrorServerLimit = 2
	ErrorServerFatal = 3
	ErrorUnknown     = 4
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// {"port": 59830, "function": 0, "ip": "180.97.81.180", "error": 0}
// {"function":0}
// {"function": 7, "error": 0}
// {"function":5,"data":"xss"}
// tcp连接 {"port": 54377, "function": 0, "ip": "180.97.81.180", "error": 0}
// 新连接 {"function": 1, "address_str": "1.28.200.109:45950", "error": 0}
// tcp消息 {"address_str": "1.28.200.109:48733", "function": 3, "data": "xss", "error": 0}
// 客户端踢用户下线 {"function":4,"address_str":"1.28.200.109:47470"}
// 服务端返回踢下线请求 {"function": 2, "address_str": "1.28.200.109:47470", "error": 0}

type Message struct {
	Function   int    `json:"function"`
	Ip         string `json:"ip"`
	Port       int    `json:"port"`
	Error      int    `json:"error"`
	Data       string `json:"data"`
	AddressStr string `json:"address_str"`
}

type Client struct {
	// 客户端关闭事件
	closeCh chan struct{}
	// 当前网页websocket连接
	wsConn *websocket.Conn
	// tcp listener
	tcpListener net.Listener
	// tcp server所有连接
	tcpConnections map[net.Conn]string
	// tcp端口
	tcpPort int
	// 最近一次消息
	lastUpdate time.Time
	// 保护tcpConnections
	sync.Mutex
}

func (c *Client) gc() {
	defer func() {
		err := recover()
		log.Println("recover", err)
	}()
	ticker := time.NewTicker(time.Second)
	for {
		select {
		case <-c.closeCh:
			log.Println("client close")
		case <-ticker.C:
			log.Println("client gc")
			if time.Now().Sub(c.lastUpdate) > 3*time.Minute {
				message := Message{
					Function: FunctionServerIdleTimeLimit,
					Error:    0,
				}

				c.wsConn.WriteJSON(&message)

				delete(ports, c.tcpPort)
				c.tcpListener.Close()
				c.wsConn.Close()
				if c.closeCh != nil {
					close(c.closeCh)
					c.closeCh = nil
				}
				return
			}
		}
	}
}

func NewClient(closeCh chan struct{}, wsConn *websocket.Conn, tcpConnections map[net.Conn]string) *Client {
	client := &Client{closeCh: closeCh, wsConn: wsConn, tcpConnections: tcpConnections}
	client.lastUpdate = time.Now()
	go client.gc()
	return client
}

var ip = flag.String("ip", "119.45.32.75", "ip")
var port = flag.String("port", "8080", "port")

func main() {
	flag.Parse()

	rand.Seed(time.Now().Unix())

	// serve static files
	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/", fs)

	http.HandleFunc("/ws", ws)

	go func() {
		ticker := time.NewTicker(3 * time.Second)
		for {
			<-ticker.C
			log.Println(ports)
		}
	}()

	err := http.ListenAndServe(":"+*port, nil)
	if err != nil {
		log.Fatal(err)
	}
}

func ws(w http.ResponseWriter, r *http.Request) {
	var (
		err  error
		conn *websocket.Conn
		port int
	)
	conn, err = upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	client := NewClient(make(chan struct{}), conn, make(map[net.Conn]string))
	port, err = findAvailablePort()
	if err != nil {
		log.Println(err.Error())
		if client.closeCh != nil {
			close(client.closeCh)
			client.closeCh = nil
		}
	}
	err = conn.WriteJSON(&Message{
		Function: 0,
		Ip:       *ip,
		Port:     port,
		Error:    0,
	})
	if err != nil {
		if client.closeCh != nil {
			close(client.closeCh)
			client.closeCh = nil
		}
	}

	go read(conn, client)
	go startTCPServer(port, client)
}

func read(conn *websocket.Conn, client *Client) {
	defer func() {
		err := recover()
		log.Println("recover", err)
	}()
	for {
		var message Message
		if err := conn.ReadJSON(&message); err != nil {
			log.Println("read message from client:", err)
			if client.closeCh != nil {
				close(client.closeCh)
				client.closeCh = nil
			}
			return
		}

		client.Lock()
		client.lastUpdate = time.Now()
		client.Unlock()

		if message.Function == FunctionCreateServer {
			log.Println("client register")
		} else if message.Function == FunctionWsNormalMsg {
			for tcpConn := range client.tcpConnections {
				_, err := tcpConn.Write([]byte(message.Data))
				if err != nil {
					log.Println("error writing to tcp connection")
					delete(client.tcpConnections, tcpConn)
				}
			}
		} else if message.Function == FunctionKickClient {
			log.Println("kick client", message.AddressStr)

			for tcpConn, address := range client.tcpConnections {
				if address == message.AddressStr {
					log.Println("found & delete", message.AddressStr)
					delete(client.tcpConnections, tcpConn)

					message := Message{
						Function:   FunctionClientDisconnected,
						AddressStr: message.AddressStr,
					}
					if err := conn.WriteJSON(&message); err != nil {
						log.Println("error writing kick client")
					}

					tcpConn.Close()
				}
			}
		}
	}
}

func startTCPServer(port int, client *Client) {
	defer func() {
		err := recover()
		log.Println("recover", err)
	}()
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Println(err.Error())
		return
	}

	client.tcpListener = listener

	fmt.Println("Listening on :", port)

	client.tcpPort = port

	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Println("Error accepting: ", err)
			break
		}

		fmt.Printf("Received message %s -> %s \n", conn.RemoteAddr(), conn.LocalAddr())

		client.tcpConnections[conn] = conn.RemoteAddr().String()

		// tcp连接 {"port": 54377, "function": 0, "ip": "180.97.81.180", "error": 0}
		//var message Message
		//ip := strings.Split(conn.RemoteAddr().String(), ":")[0]
		//port, _ := strconv.Atoi(strings.Split(conn.RemoteAddr().String(), ":")[1])
		//message = Message{
		//	Function: FunctionCreateServer,
		//	Ip:       ip,
		//	Port:     port,
		//	Error:    0,
		//}
		//if err := client.wsConn.WriteJSON(&message); err != nil {
		//	log.Println(err.Error())
		//}

		// 新连接 {"function": 1, "address_str": "1.28.200.109:45950", "error": 0}
		message := Message{
			Function:   FunctionClientConnected,
			AddressStr: conn.RemoteAddr().String(),
			Error:      0,
		}
		if err := client.wsConn.WriteJSON(&message); err != nil {
			log.Println(err.Error())
		}

		go handleConn(conn, client)
	}
}

func handleConn(conn net.Conn, client *Client) {
	defer func() {
		conn.Close()
		err := recover()
		log.Println("recover", err)
	}()

	log.Println("begin to handle connection", conn.RemoteAddr().String())

	for {
		buf := make([]byte, 1024)
		n, err := conn.Read(buf)

		if err != nil {
			log.Println("tcp connection disconnected")
			delete(client.tcpConnections, conn)
			message := Message{
				Function:   FunctionClientDisconnected,
				AddressStr: conn.RemoteAddr().String(),
				Error:      0,
			}
			if err := client.wsConn.WriteJSON(&message); err != nil {
				log.Println("error writing server idle message")
				if client.closeCh != nil {
					close(client.closeCh)
					client.closeCh = nil
				}
				break
			}
			return
		}
		log.Println("Received message from", conn.RemoteAddr().String())

		client.Lock()
		client.lastUpdate = time.Now()
		client.Unlock()

		message := Message{
			Function:   FunctionClientNormalMsg,
			Data:       string(buf[:n]),
			AddressStr: conn.RemoteAddr().String(),
		}
		if err := client.wsConn.WriteJSON(&message); err != nil {
			log.Println("write to ws connection error", err.Error())
		}
	}
}

var ports = make(map[int]bool)

func findAvailablePort() (int, error) {
	max := PortMax - PortMin
	n := 0
	for n < max {
		port := randomPort()
		n++
		if _, ok := ports[port]; ok {
			continue
		}
		ports[port] = true
		return port, nil
	}
	return -1, errors.New("no available port")
}

func randomPort() int {
	return PortMin + rand.Intn(PortMax)
}

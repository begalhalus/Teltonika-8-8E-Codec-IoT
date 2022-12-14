package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var decodeConfig = &teltonika.DecodeConfig{IoElementsAlloc: teltonika.OnReadBuffer}

type Logger struct {
	Info  *log.Logger
	Error *log.Logger
}

type TrackersHub interface {
	SendPacket(imei string, packet *teltonika.Packet) error
	ListClients() []*TCPClient
}

type TCPServer struct {
	address   string
	clients   sync.Map
	logger    *Logger
	OnPacket  func(imei string, pkt *teltonika.Packet)
	OnClose   func(imei string)
	OnConnect func(imei string)
}

type TCPClient struct {
	conn net.Conn
	imei string
}

func NewTCPServer(address string) *TCPServer {
	return &TCPServer{address: address, logger: &Logger{log.Default(), log.Default()}}
}

func NewTCPServerLogger(address string, logger *Logger) *TCPServer {
	return &TCPServer{address: address, logger: logger}
}

func (r *TCPServer) Run() error {
	logger := r.logger

	addr, err := net.ResolveTCPAddr("tcp", r.address)
	if err != nil {
		return fmt.Errorf("tcp address resolve error (%v)", err)
	}

	listener, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return fmt.Errorf("tcp listener create error (%v)", err)
	}

	defer func() {
		_ = listener.Close()
	}()

	logger.Info.Println("tcp server listening at " + r.address)

	for {
		conn, err := listener.Accept()
		if err != nil {
			return fmt.Errorf("tcp connection accept error (%v)", err)
		}
		go r.handleConnection(conn)
	}
}

func (r *TCPServer) SendPacket(imei string, packet *teltonika.Packet) error {
	clientRaw, ok := r.clients.Load(imei)
	if !ok {
		return fmt.Errorf("client with imei '%s' not found", imei)
	}
	client := clientRaw.(*TCPClient)

	buf, err := teltonika.EncodePacket(packet)
	if err != nil {
		return err
	}

	if _, err = client.conn.Write(buf); err != nil {
		return err
	}

	return nil
}

func (r *TCPServer) ListClients() []*TCPClient {
	clients := make([]*TCPClient, 0, 10)
	r.clients.Range(func(key, value any) bool {
		clients = append(clients, value.(*TCPClient))
		return true
	})
	return clients
}

func (r *TCPServer) handleConnection(conn net.Conn) {
	logger := r.logger
	client := &TCPClient{conn, ""}
	imei := ""

	addr := conn.RemoteAddr().String()

	defer func(conn net.Conn) {
		if r.OnClose != nil && imei != "" {
			r.OnClose(imei)
		}
		if imei != "" {
			logger.Info.Printf("[%s]: disconnected", imei)
			r.clients.Delete(imei)
		} else {
			logger.Info.Printf("[%s]: disconnected", addr)
		}

		if err := conn.Close(); err != nil {
			logger.Error.Printf("[%s]: connection close error (%v)", addr, err)
		}
	}(conn)

	logger.Info.Printf("[%s]: connected", addr)

	buf := make([]byte, 100)
	size, err := conn.Read(buf) // Read imei
	if err != nil {
		logger.Error.Printf("[%s]: connection read error (%v)", addr, err)
		return
	}
	if size < 2 {
		logger.Error.Printf("[%s]: invalid first message (read: %s)", addr, hex.EncodeToString(buf))
		return
	}
	imeiLen := int(binary.BigEndian.Uint16(buf[:2]))
	buf = buf[2:]

	if len(buf) < imeiLen {
		logger.Error.Printf("[%s]: invalid imei size (read: %s)", addr, hex.EncodeToString(buf))
		return
	}

	imei = strings.TrimSpace(string(buf[:imeiLen]))
	client.imei = imei

	if r.OnConnect != nil {
		r.OnConnect(imei)
	}

	r.clients.Store(imei, client)

	logger.Info.Printf("[%s]: imei - %s", addr, client.imei)

	if _, err = conn.Write([]byte{1}); err != nil {
		logger.Error.Printf("[%s]: error writing ack (%v)", client.imei, err)
		return
	}

	readBuffer := make([]byte, 1300)
	for {
		if err = conn.SetReadDeadline(time.Now().Add(time.Minute * 15)); err != nil {
			logger.Error.Printf("[%s]: SetReadDeadline error (%v)", imei, err)
			return
		}
		read, res, err := teltonika.DecodeTCPFromReaderBuf(conn, readBuffer, decodeConfig)
		if err != nil {
			logger.Error.Printf("[%s]: packet decode error (%v)", imei, err)
			return
		}

		if res.Response != nil {
			if _, err = conn.Write(res.Response); err != nil {
				logger.Error.Printf("[%s]: error writing response (%v)", imei, err)
				return
			}
		}

		logger.Info.Printf("[%s]: message: %s", imei, hex.EncodeToString(readBuffer[:read]))
		jsonData, err := json.Marshal(res.Packet)
		if err != nil {
			logger.Error.Printf("[%s]: decoder result marshaling error (%v)", imei, err)
		}
		logger.Info.Printf("[%s]: decoded: %s", imei, string(jsonData))

		if r.OnPacket != nil {
			r.OnPacket(imei, res.Packet)
		}
	}
}

type HTTPServer struct {
	address  string
	hub      TrackersHub
	respChan *sync.Map
	logger   *Logger
}

func NewHTTPServer(address string, hub TrackersHub) *HTTPServer {
	return &HTTPServer{address: address, respChan: &sync.Map{}, hub: hub}
}

func NewHTTPServerLogger(address string, hub TrackersHub, logger *Logger) *HTTPServer {
	return &HTTPServer{address: address, respChan: &sync.Map{}, hub: hub, logger: logger}
}

func (hs *HTTPServer) Run() error {
	logger := hs.logger

	handler := http.NewServeMux()

	handler.HandleFunc("/cmd", hs.handleCmd)

	handler.HandleFunc("/list-clients", hs.listClients)

	logger.Info.Println("http server listening at " + hs.address)

	err := http.ListenAndServe(hs.address, handler)
	if err != nil {
		return fmt.Errorf("http listen error (%v)", err)
	}
	return nil
}

func (hs *HTTPServer) WriteMessage(imei string, message *teltonika.Message) {
	ch, ok := hs.respChan.Load(imei)
	if ok {
		select {
		case ch.(chan *teltonika.Message) <- message:
		}
	}
}

func (hs *HTTPServer) listClients(w http.ResponseWriter, _ *http.Request) {
	for _, client := range hs.hub.ListClients() {
		_, err := w.Write([]byte(client.conn.RemoteAddr().String() + " - " + client.imei + "\n"))
		if err != nil {
			return
		}
	}
	w.WriteHeader(200)
}

func (hs *HTTPServer) handleCmd(w http.ResponseWriter, r *http.Request) {
	logger := hs.logger

	params := r.URL.Query()
	imei := params.Get("imei")
	buf := make([]byte, 512)
	n, _ := r.Body.Read(buf)
	cmd := string(buf[:n])

	packet := &teltonika.Packet{
		CodecID:  teltonika.Codec12,
		Data:     nil,
		Messages: []teltonika.Message{{Type: teltonika.TypeCommand, Text: strings.TrimSpace(cmd)}},
	}

	result := make(chan *teltonika.Message, 1)
	defer close(result)
	for {
		if _, loaded := hs.respChan.LoadOrStore(imei, result); !loaded {
			break
		}
		time.Sleep(time.Millisecond * 100)
	}

	defer hs.respChan.Delete(imei)

	if err := hs.hub.SendPacket(imei, packet); err != nil {
		logger.Error.Printf("send packet error (%v)", err)
		_, err = w.Write([]byte(err.Error() + "\n"))
		if err != nil {
			logger.Error.Printf("http write error (%v)", err)
		} else {
			w.WriteHeader(400)
		}
	} else {
		logger.Info.Printf("command '%s' sent to '%s'", cmd, imei)
		ticker := time.NewTimer(time.Second * 90)
		defer ticker.Stop()

		select {
		case msg := <-result:
			_, err = w.Write([]byte(msg.Text + "\n"))
		case <-ticker.C:
			_, err = w.Write([]byte("tracker response timeout exceeded\n"))
		}

		if err != nil {
			logger.Error.Printf("http write error (%v)", err)
		} else {
			w.WriteHeader(200)
		}
	}
}

func main() {
	var httpAddress string
	var tcpAddress string
	var outHook string
	flag.StringVar(&tcpAddress, "address", "0.0.0.0:8080", "tcp server address")
	flag.StringVar(&httpAddress, "http", "0.0.0.0:8081", "http server address")
	flag.StringVar(&outHook, "hook", "http://localhost:5000/api/v1/metric", "output hook")
	flag.Parse()

	logger := &Logger{
		Info:  log.New(os.Stdout, "INFO: ", log.Ldate|log.Ltime),
		Error: log.New(os.Stdout, "ERROR: ", log.Ldate|log.Ltime|log.Lshortfile),
	}

	serverTcp := NewTCPServerLogger(tcpAddress, logger)
	serverHttp := NewHTTPServerLogger(httpAddress, serverTcp, logger)

	serverTcp.OnPacket = func(imei string, pkt *teltonika.Packet) {
		if pkt.Messages != nil && len(pkt.Messages) > 0 {
			serverHttp.WriteMessage(imei, &pkt.Messages[0])
		}
		if pkt.Data != nil {
			go hookSend(outHook, imei, pkt, logger)
		}
	}

	go func() {
		panic(serverTcp.Run())
	}()
	panic(serverHttp.Run())
}

func buildJsonPacket(imei string, pkt *teltonika.Packet) []byte {
	if pkt.Data == nil {
		return nil
	}
	gpsFrames := make([]interface{}, 0)
	for _, frame := range pkt.Data {
		gpsFrames = append(gpsFrames, map[string]interface{}{
			"timestamp": int64(frame.TimestampMs / 1000.0),
			"lat":       frame.Lat,
			"lon":       frame.Lng,
		})
	}
	if len(gpsFrames) == 0 {
		return nil
	}
	values := map[string]interface{}{
		"deveui": imei,
		"time":   time.Now().String(),
		"frames": map[string]interface{}{
			"gps": gpsFrames,
		},
	}
	jsonValue, _ := json.Marshal(values)
	return jsonValue
}

func hookSend(outHook string, imei string, pkt *teltonika.Packet, logger *Logger) {
	jsonValue := buildJsonPacket(imei, pkt)
	if jsonValue == nil {
		return
	}
	res, err := http.Post(outHook, "application/json", bytes.NewBuffer(jsonValue))
	if err != nil {
		logger.Error.Printf("http post error (%v)", err)
	} else {
		logger.Info.Printf("packet sent to output hook, status: %s", res.Status)
	}
}

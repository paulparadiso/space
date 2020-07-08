package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/s4y/reserve"
)

var baseDirectory string = filepath.Join(os.Getenv("GOPATH"), "src/github.com/s4y/space/")

type ClientMessage struct {
	Type string          `json:"type"`
	Body json.RawMessage `json:"body"`
}

func MakeClientMessage(t string, message interface{}) ClientMessage {
	body, _ := json.Marshal(message)
	return ClientMessage{t, body}
}

type Vec2 [2]float64

type GuestState struct {
	Position Vec2 `json:"position"`
	Look     Vec2 `json:"look"`
}

type Guest struct {
	GuestState
	JoinTime int `json:"joinTime"`
	read     chan interface{}
	write    chan interface{}
	ctx      context.Context
	cancel   context.CancelFunc
}

func MakeGuest(ctx context.Context, conn *websocket.Conn) *Guest {
	childCtx, cancel := context.WithCancel(ctx)
	guest := &Guest{
		read:   make(chan interface{}),
		write:  make(chan interface{}, 100),
		ctx:    childCtx,
		cancel: cancel,
	}

	go func() {
		for msg := range guest.read {
			conn.WriteJSON(msg)
		}
	}()

	go func() {
		for msg := range guest.write {
			conn.WriteJSON(msg)
		}
	}()

	go func() {
		<-childCtx.Done()
		conn.Close()
		close(guest.read)
		// close(guest.write)
	}()

	return guest
}

func (g *Guest) Read(msg interface{}) (interface{}, error) {
	if msg, ok := <-g.read; ok {
		return msg, nil
	} else {
		return nil, errors.New("read from closed websocket")
	}
}

func (g *Guest) Write(msg interface{}) error {
	select {
	case <-g.ctx.Done():
		return errors.New("write to closed websocket")
	case g.write <- msg:
		return nil
	default:
		g.cancel()
		return errors.New(fmt.Sprint("full WebSocket, dropping connection."))
	}
}

type World struct {
	mutex  sync.Mutex
	seq    uint32
	Guests map[uint32]*Guest `json:"guests"`
}

func NewWorld() World {
	return World{
		Guests: map[uint32]*Guest{},
	}
}

func MakeGuestUpdateMessage(id uint32, state Guest) interface{} {
	return MakeClientMessage("guestUpdate", struct {
		Id    uint32 `json:"id"`
		State Guest  `json:"state"`
	}{id, state})
}

func (w *World) broadcast(m interface{}, skip uint32) {
	for k, v := range w.Guests {
		if k == skip {
			continue
		}
		v.Write(m)
	}
}

func (w *World) SendTo(to uint32, m interface{}) {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	if g, ok := w.Guests[to]; ok {
		g.Write(m)
	}
}

func (w *World) AddGuest(ctx context.Context, g *Guest) uint32 {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	w.seq += 1
	seq := w.seq

	g.Write(MakeClientMessage("hello", struct {
		Seq uint32 `json:"seq"`
	}{seq}))

	for k, v := range w.Guests {
		g.Write(MakeGuestUpdateMessage(k, *v))
	}
	w.Guests[seq] = g
	go w.UpdateGuest(seq)
	go func() {
		<-ctx.Done()
		w.RemoveGuest(seq)
	}()
	return seq
}

func (w *World) BroadcastFrom(seq uint32, message interface{}) {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	w.broadcast(message, seq)
}

func (w *World) UpdateGuest(seq uint32) {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	g := w.Guests[seq]
	w.broadcast(MakeGuestUpdateMessage(seq, *g), seq)
}

func (w *World) RemoveGuest(seq uint32) {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	w.broadcast(MakeClientMessage(
		"guestLeaving",
		struct {
			Id uint32 `json:"id"`
		}{seq}), seq)
	delete(w.Guests, seq)
}

func readConfig() {
	configFile, err := os.Open(filepath.Join(baseDirectory, "static/config.json"))
	if err != nil {
		panic(err)
	}
	if err := json.NewDecoder(configFile).Decode(&config); err != nil {
		panic(err)
	}
}

var world World = NewWorld()
var partyLine *WebRTCPartyLine
var config struct {
	RTCConfiguration json.RawMessage `json:"rtcConfiguration"`
}

type Knob struct {
	Name  string      `json:"name"`
	Value interface{} `json:"value"`
}

var knobsMutex sync.RWMutex
var knobs map[string]interface{} = make(map[string]interface{})

func startManagementServer() {
	mux := http.NewServeMux()
	mux.Handle("/", reserve.FileServer(http.Dir(filepath.Join(baseDirectory, "static-management"))))

	managementAddr := "127.0.0.1:8034"
	fmt.Printf("Management UI (only) at http://%s/\n", managementAddr)
	server := http.Server{Addr: managementAddr, Handler: mux}

	upgrader := websocket.Upgrader{}

	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		// ctx := r.Context()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		var msg ClientMessage
		for {
			if err = conn.ReadJSON(&msg); err != nil {
				break
			}
			switch msg.Type {
			case "setKnob":
				var knob Knob
				if err := json.Unmarshal(msg.Body, &knob); err != nil {
					fmt.Println("knob unmarshal err", err)
				}
				knobsMutex.Lock()
				knobs[knob.Name] = knob.Value
				knobsMutex.Unlock()
				world.BroadcastFrom(0, MakeClientMessage("knob", knob))
			default:
				fmt.Println("unknown message:", msg)
			}
		}
	})

	log.Fatal(server.ListenAndServe())
}

func main() {
	readConfig()
	partyLine = NewWebRTCPartyLine(config.RTCConfiguration)

	httpAddr := flag.String("http", "127.0.0.1:8031", "Listening address")
	flag.Parse()
	fmt.Printf("http://%s/\n", *httpAddr)

	ln, err := net.Listen("tcp", *httpAddr)
	if err != nil {
		log.Fatal(err)
	}

	upgrader := websocket.Upgrader{}

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		knobsMutex.RLock()
		for name, value := range knobs {
			conn.WriteJSON(MakeClientMessage("knob", Knob{name, value}))
		}
		knobsMutex.RUnlock()

		guest := MakeGuest(ctx, conn)
		// fmt.Println("GO ADD")
		seq := world.AddGuest(ctx, guest)
		// fmt.Println("GO UPDATE")
		world.UpdateGuest(seq)
		// fmt.Println("DONE UPDATE")

		rtcPeer := WebRTCPartyLinePeer{
			UserInfo: seq,
			SendToPeer: func(message interface{}) {
				world.SendTo(seq, MakeClientMessage("rtc", struct {
					From    uint32      `json:"from"`
					Message interface{} `json:"message"`
				}{0, message}))
			},
			SendMidMapping: func(mapping map[uint32][]string) {
				world.SendTo(seq, MakeClientMessage("midMapping", mapping))
			},
		}

		// fmt.Println("GO ADD PEER")
		if err := partyLine.AddPeer(ctx, &rtcPeer); err != nil {
			fmt.Println("err creating peerconnection ", seq, err)
			return
		}
		// fmt.Println("DONE ADD PEER")

		var msg ClientMessage
		for {
			if err = conn.ReadJSON(&msg); err != nil {
				break
			}
			switch msg.Type {
			case "state":
				var state GuestState
				err := json.Unmarshal(msg.Body, &state)
				if err != nil {
					fmt.Println(err)
					break
				}
				guest.GuestState = state
				world.UpdateGuest(seq)
			case "rtc":
				var messageIn struct {
					To      uint32          `json:"to"`
					Message json.RawMessage `json:"message"`
				}
				err := json.Unmarshal(msg.Body, &messageIn)
				if err != nil {
					fmt.Println(err)
					break
				}
				if err := rtcPeer.HandleMessage(messageIn.Message); err != nil {
					fmt.Println("malformed rtc message from", seq, string(messageIn.Message), err)
				}
				// messageOut := struct {
				// 	From    uint32          `json:"from"`
				// 	Message json.RawMessage `json:"message"`
				// }{seq, messageIn.Message}
				// world.SendTo(messageIn.To, MakeClientMessage("rtc", messageOut))
			default:
				fmt.Println("unknown message:", msg)
			}
		}
		return
	})
	http.Handle("/media/music", makeMusicHandler())
	// http.Handle("/astream/", http.FileServer(http.Dir(".")))
	http.Handle("/", reserve.FileServer(http.Dir(filepath.Join(baseDirectory, "static"))))

	go startManagementServer()
	log.Fatal(http.Serve(ln, nil))
}
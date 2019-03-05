package main

import (
	"net"
	"sync"

	"fmt"

	"math/rand"

	"time"

	"reflect"

	"crypto/tls"

	"github.com/xiaonanln/goworld/engine/common"
	"github.com/xiaonanln/goworld/engine/config"
	"github.com/xiaonanln/goworld/engine/gwioutil"
	"github.com/xiaonanln/goworld/engine/gwlog"
	"github.com/xiaonanln/goworld/engine/netutil"
	"github.com/xiaonanln/goworld/engine/post"
	"github.com/xiaonanln/goworld/engine/proto"
	"golang.org/x/net/websocket"
)

var (
	tlsConfig = &tls.Config{
		InsecureSkipVerify: true,
	}
)

// ClientBot is  a client bot representing a game client
type ClientBot struct {
	sync.Mutex

	id     int
	player *clientEntity

	waiter           *sync.WaitGroup
	waitAllConnected *sync.WaitGroup
	conn             *proto.GoWorldConnection
	noEntitySync     bool
	packetQueue      chan proto.Message
}

func newClientBot(id int, waiter *sync.WaitGroup, waitAllConnected *sync.WaitGroup) *ClientBot {
	return &ClientBot{
		id:               id,
		waiter:           waiter,
		waitAllConnected: waitAllConnected,
		packetQueue:      make(chan proto.Message),
	}
}

func (bot *ClientBot) String() string {
	return fmt.Sprintf("ClientBot<%d>", bot.id)
}

func (bot *ClientBot) run() {
	defer bot.waiter.Done()

	gwlog.Infof("%s is running ...", bot)

	desiredGates := config.GetDeployment().DesiredGates
	// choose a random gateid
	gateid := uint16(rand.Intn(desiredGates) + 1)
	gwlog.Debugf("%s is connecting to gate %d", bot, gateid)
	cfg := config.GetGate(gateid)

	var netconn net.Conn
	var err error
	for { // retry for ever
		netconn, err = bot.connectServer(cfg)
		if err != nil {
			Errorf("%s: connect failed: %s", bot, err)
			time.Sleep(time.Second * time.Duration(1+rand.Intn(10)))
			continue
		}
		// connected , ok
		break
	}

	gwlog.Infof("connected: %s", netconn.RemoteAddr())
	bot.conn = proto.NewGoWorldConnection(netutil.NewBufferedConnection(netutil.NetConnection{netconn}), cfg.CompressConnection, cfg.CompressFormat)
	defer bot.conn.Close()

	go bot.recvLoop()
	bot.waitAllConnected.Done()

	bot.waitAllConnected.Wait()
	bot.loop()
}

func (bot *ClientBot) connectServer(cfg *config.GateConfig) (net.Conn, error) {
	return bot.connectServerByWebsocket(cfg)
}

func (bot *ClientBot) connectServerByWebsocket(cfg *config.GateConfig) (net.Conn, error) {
	originProto := "http"
	wsProto := "ws"
	if cfg.EncryptConnection {
		originProto = "https"
		wsProto = "wss"
	}
	_, httpPort, err := net.SplitHostPort(cfg.HTTPAddr)
	if err != nil {
		gwlog.Fatalf("can not parse host:port: %s", cfg.HTTPAddr)
	}

	origin := fmt.Sprintf("%s://%s:%s/", originProto, serverHost, httpPort)
	wsaddr := fmt.Sprintf("%s://%s:%s/ws", wsProto, serverHost, httpPort)

	if cfg.EncryptConnection {
		dialCfg, err := websocket.NewConfig(wsaddr, origin)
		if err != nil {
			return nil, err
		}
		dialCfg.TlsConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
		return websocket.DialConfig(dialCfg)
	} else {
		return websocket.Dial(wsaddr, "", origin)
	}
}

func (bot *ClientBot) recvLoop() {
	var msgtype proto.MsgType

	for {
		pkt, err := bot.conn.Recv(&msgtype)
		if pkt != nil {
			//fmt.Fprintf(os.Stderr, "P")
			bot.packetQueue <- proto.Message{msgtype, pkt}
		} else if err != nil && !gwioutil.IsTimeoutError(err) {
			// bad error
			Errorf("%s: client recv packet failed: %v", bot, err)
			break
		}
	}
}

func (bot *ClientBot) loop() {
	ticker := time.Tick(time.Millisecond * 100)
	for {
		select {
		case item := <-bot.packetQueue:
			//fmt.Fprintf(os.Stderr, "p")
			bot.handlePacket(item.MsgType, item.Packet)
			item.Packet.Release()
			break
		case <-ticker:
			bot.conn.Flush("ClientBot")
			post.Tick()
			break
		}
	}
}

func (bot *ClientBot) handlePacket(msgtype proto.MsgType, packet *netutil.Packet) {
	defer func() {
		err := recover()
		if err != nil {
			gwlog.TraceError("handle packet failed: %v", err)
		}
	}()

	bot.Lock()
	defer bot.Unlock()

	gwlog.Debugf("client handle packet: msgtype=%v, payload=%v", msgtype, packet.Payload())

	if msgtype >= proto.MT_REDIRECT_TO_GATEPROXY_MSG_TYPE_START && msgtype <= proto.MT_REDIRECT_TO_GATEPROXY_MSG_TYPE_STOP {
		_ = packet.ReadUint16()
		_ = packet.ReadClientID() // TODO: strip these two fields ? seems a little difficult, maybe later.
	}

	if msgtype == proto.MT_CALL_FILTERED_CLIENTS {
		_ = packet.ReadOneByte() // ignore op
		_ = packet.ReadVarStr()  // ignore key
		_ = packet.ReadVarStr()  // ignore val
		method := packet.ReadVarStr()
		args := packet.ReadArgs()
		if bot.player == nil {
			gwlog.Warnf("Player not found while calling filtered client")
			return
		}

		bot.callEntityMethod(method, args)
	} else if msgtype == proto.MT_CREATE_ENTITY_ON_CLIENT {
		isPlayer := packet.ReadBool()
		entityID := packet.ReadEntityID()
		typeName := packet.ReadVarStr()

		// x,y,z,yaw
		packet.ReadFloat32()
		packet.ReadFloat32()
		packet.ReadFloat32()
		packet.ReadFloat32()
		var clientData map[string]interface{}
		packet.ReadData(&clientData)
		bot.createEntity(typeName, entityID, isPlayer, clientData)
	} else if msgtype == proto.MT_DESTROY_ENTITY_ON_CLIENT {
		typeName := packet.ReadVarStr()
		entityID := packet.ReadEntityID()
		if !quiet {
			gwlog.Debugf("Destroy e %s.%s", typeName, entityID)
		}
		bot.destroyEntity(typeName, entityID)
	} else {
		gwlog.Panicf("unknown msgtype: %v", msgtype)
	}
}

func (bot *ClientBot) callEntityMethod(method string, args [][]byte) {
	if bot.player == nil {
		if method != "OnLogin" {
			// Method OnLogin might be called when Account is already destroyed
			Errorf("%s: entity is not found while calling method %s(%v)", bot, method, args)
		}

		return
	}

	methodVal := reflect.ValueOf(bot.player).MethodByName(method)
	if !methodVal.IsValid() {
		Errorf("%s： client method %s is not found", bot, method)
		return
	}

	methodType := methodVal.Type()
	in := make([]reflect.Value, len(args))

	for i, arg := range args {
		argType := methodType.In(i)
		argValPtr := reflect.New(argType)
		netutil.MSG_PACKER.UnpackMsg(arg, argValPtr.Interface())
		in[i] = reflect.Indirect(argValPtr)
	}
	methodVal.Call(in)
}

func (bot *ClientBot) username() string {
	return fmt.Sprintf("test%d", bot.id)
}

func (bot *ClientBot) password() string {
	return "123456"
}

// CallServer calls server method of target e
func (bot *ClientBot) CallServer(id common.EntityID, method string, args []interface{}) {
	if !quiet {
		gwlog.Debugf("%s call server: %s.%s%v", bot, id, method, args)
	}
	bot.conn.SendCallEntityMethodFromClient(id, method, args)
}

func Errorf(fmt string, args ...interface{}) {
	if strictMode {
		gwlog.Fatalf(fmt, args...)
	} else {
		gwlog.Errorf(fmt, args...)
	}

}

func (bot *ClientBot) createEntity(typeName string, entityID common.EntityID, isPlayer bool, clientData map[string]interface{}) {
	gwlog.Debugf("%s: create entity %s<%s>, isPlayer=%v", bot, typeName, entityID, isPlayer)
	if isPlayer {
		e := newClientEntity(bot, typeName, entityID, isPlayer, clientData)
		if bot.player != nil {
			gwlog.TraceError("%s.createEntity: creating player %S, but player is already set to %s", bot, e, bot.player)
		}
		bot.player = e
	}
}

func (bot *ClientBot) destroyEntity(typeName string, entityID common.EntityID) {
	gwlog.Debugf("%s: destroy entity %s<%s>", bot, typeName, entityID)
	if bot.player != nil && bot.player.ID == entityID {
		bot.player.Destroy()
		bot.player = nil
	}
}

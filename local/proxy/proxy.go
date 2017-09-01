package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/yinqiwen/gsnova/common/helper"
	"github.com/yinqiwen/gsnova/common/logger"
	"github.com/yinqiwen/gsnova/common/mux"
	"github.com/yinqiwen/gsnova/local"
	"github.com/yinqiwen/gsnova/local/hosts"
	"github.com/yinqiwen/pmux"
)

var proxyHome string

type InternalEventMonitor func(code int, desc string) error

type ProxyFeatureSet struct {
	AutoExpire bool
}

type Proxy interface {
	//PrintStat(w io.Writer)
	CreateMuxSession(server string, conf *ProxyChannelConfig) (mux.MuxSession, error)
	Features() ProxyFeatureSet
}

func init() {
	proxyHome = "."
}

var proxyTypeTable map[string]reflect.Type = make(map[string]reflect.Type)
var clientConfName = "client.json"
var hostsConfName = "hosts.json"

func allowedSchema() []string {
	schames := []string{}
	for schema := range proxyTypeTable {
		if schema != directProxyChannelName {
			schames = append(schames, schema)
		}
	}
	sort.Strings(schames)
	return schames
}

func InitialPMuxConfig(conf *ProxyChannelConfig) *pmux.Config {
	cfg := pmux.DefaultConfig()
	cfg.CipherKey = []byte(GConf.Cipher.Key)
	cfg.CipherMethod = mux.DefaultMuxCipherMethod
	cfg.CipherInitialCounter = mux.DefaultMuxInitialCipherCounter
	if conf.HeartBeatPeriod > 0 {
		cfg.EnableKeepAlive = true
		cfg.KeepAliveInterval = time.Duration(conf.HeartBeatPeriod) * time.Second
	} else {
		cfg.EnableKeepAlive = false
	}
	return cfg
}

type muxSessionHolder struct {
	creatTime       time.Time
	expireTime      time.Time
	muxSession      mux.MuxSession
	retiredSessions map[mux.MuxSession]bool
	server          string
	p               Proxy
	sessionMutex    sync.Mutex
	conf            *ProxyChannelConfig
}

func (s *muxSessionHolder) tryCloseRetiredSessions() {
	for retiredSession := range s.retiredSessions {
		if retiredSession.NumStreams() <= 0 {
			log.Printf("Close retired mux session since it's has no active stream.")
			retiredSession.Close()
			delete(s.retiredSessions, retiredSession)
		}
	}
}

func (s *muxSessionHolder) dumpStat(w io.Writer) {
	s.sessionMutex.Lock()
	defer s.sessionMutex.Unlock()
	s.tryCloseRetiredSessions()
	fmt.Fprintf(w, "Server:%s, CreateTime:%v, RetireTime:%v, RetireSessionNum:%v\n", s.server, s.creatTime.Format("15:04:05"), s.expireTime.Format("15:04:05"), len(s.retiredSessions))
}

func (s *muxSessionHolder) close() {
	s.sessionMutex.Lock()
	defer s.sessionMutex.Unlock()

	if nil != s.muxSession {
		s.muxSession.Close()
		s.muxSession = nil
	}
}

func (s *muxSessionHolder) getNewStream() (mux.MuxStream, error) {
	s.sessionMutex.Lock()
	defer s.sessionMutex.Unlock()
	defer func() {
		s.tryCloseRetiredSessions()
	}()
	if nil != s.muxSession && !s.expireTime.IsZero() && s.expireTime.Before(time.Now()) {
		s.retiredSessions[s.muxSession] = true
		s.muxSession = nil
	}
	if nil == s.muxSession {
		s.init(false)
	}
	if nil == s.muxSession {
		return nil, pmux.ErrSessionShutdown
	}
	return s.muxSession.OpenStream()
}

func (s *muxSessionHolder) init(lock bool) error {
	if lock {
		s.sessionMutex.Lock()
		defer s.sessionMutex.Unlock()
	}
	if nil != s.muxSession {
		return nil
	}
	session, err := s.p.CreateMuxSession(s.server, s.conf)
	if nil == err && nil != session {
		authStream, err := session.OpenStream()
		if nil != err {
			return err
		}
		counter := uint64(helper.RandBetween(0, math.MaxInt32))
		cipherMethod := GConf.Cipher.Method
		if strings.HasPrefix(s.server, "https://") || strings.HasPrefix(s.server, "wss://") || strings.HasPrefix(s.server, "tls://") {
			cipherMethod = "none"
		}
		authReq := &mux.AuthRequest{
			User:           GConf.User,
			CipherCounter:  counter,
			CipherMethod:   cipherMethod,
			CompressMethod: s.conf.Compressor,
		}
		err = authStream.Auth(authReq)
		if nil != err {
			return err
		}
		if psession, ok := session.(*mux.ProxyMuxSession); ok {
			err = psession.Session.ResetCryptoContext(cipherMethod, counter)
			if nil != err {
				log.Printf("[ERROR]Failed to reset cipher context with reason:%v", err)
				return err
			}
		}
		s.creatTime = time.Now()
		s.muxSession = session
		features := s.p.Features()
		if features.AutoExpire {
			expireAfter := 1800
			if s.conf.ReconnectPeriod > 0 {
				expireAfter = helper.RandBetween(s.conf.ReconnectPeriod-s.conf.RCPRandomAdjustment, s.conf.ReconnectPeriod+s.conf.RCPRandomAdjustment)
			}
			log.Printf("Mux session woulde expired after %d seconds.", expireAfter)
			s.expireTime = time.Now().Add(time.Duration(expireAfter) * time.Second)
		}
		return nil
	}
	if nil == err {
		err = fmt.Errorf("Empty error to create session")
	}
	return err
}

type proxyChannel struct {
	Conf     ProxyChannelConfig
	sessions map[*muxSessionHolder]bool
	//proxies  map[Proxy]map[string]bool
}

func (ch *proxyChannel) createMuxSessionByProxy(p Proxy, server string) (*muxSessionHolder, error) {
	holder := &muxSessionHolder{
		conf:            &ch.Conf,
		p:               p,
		server:          server,
		retiredSessions: make(map[mux.MuxSession]bool),
	}
	err := holder.init(true)
	if nil == err {
		ch.sessions[holder] = true
		return holder, nil
	}
	return nil, err
}

func (ch *proxyChannel) getMuxStream() (stream mux.MuxStream, err error) {
	for holder := range ch.sessions {
		stream, err = holder.getNewStream()
		if nil != err {
			if err == pmux.ErrSessionShutdown {
				holder.close()
			}
			log.Printf("Try to get next session since current session failed to open new stream with err:%v", err)
		} else {
			return
		}
	}
	if nil == stream {
		err = fmt.Errorf("Create mux porxy stream failed")
	}
	return
}

func newProxyChannel() *proxyChannel {
	channel := &proxyChannel{}
	channel.sessions = make(map[*muxSessionHolder]bool)
	return channel
}

var proxyChannelTable = make(map[string]*proxyChannel)
var proxyChannelMutex sync.Mutex

func RegisterProxyType(str string, p Proxy) error {
	rt := reflect.TypeOf(p)
	if rt.Kind() == reflect.Ptr {
		rt = rt.Elem()
	}
	proxyTypeTable[str] = rt
	return nil
}

func getMuxStreamByChannel(name string) (mux.MuxStream, *ProxyChannelConfig, error) {
	proxyChannelMutex.Lock()
	pch, exist := proxyChannelTable[name]
	proxyChannelMutex.Unlock()
	if !exist {
		return nil, nil, fmt.Errorf("No proxy found to get mux session")
	}
	stream, err := pch.getMuxStream()
	return stream, &pch.Conf, err
}

func loadConf(conf string) error {
	if strings.HasSuffix(conf, clientConfName) {
		confdata, err := helper.ReadWithoutComment(conf, "//")
		if nil != err {
			log.Println(err)
		}
		GConf = LocalConfig{}
		err = json.Unmarshal(confdata, &GConf)
		if nil != err {
			fmt.Printf("Failed to unmarshal json:%s to config for reason:%v", string(confdata), err)
		}
		return err
	}
	err := hosts.Init(conf)
	if nil != err {
		log.Printf("Failed to init local hosts with reason:%v.", err)
	}
	return err
}

func watchConf(watcher *fsnotify.Watcher) {
	for {
		select {
		case event := <-watcher.Events:
			//log.Println("event:", event)
			if event.Op&fsnotify.Write == fsnotify.Write {
				log.Println("modified file:", event.Name)
				loadConf(event.Name)
			}
		case err := <-watcher.Errors:
			log.Println("error:", err)
		}
	}
}

func Start(home string, monitor InternalEventMonitor) error {
	confWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
		return err
	}
	clientConf := home + "/" + clientConfName
	hostsConf := home + "/" + hostsConfName
	confWatcher.Add(clientConf)
	confWatcher.Add(hostsConf)
	go watchConf(confWatcher)
	err = loadConf(clientConf)
	if nil != err {
		//log.Println(err)
		return err
	}
	loadConf(hostsConf)

	proxyHome = home
	GConf.init()
	logger.InitLogger(GConf.Log)

	log.Printf("Allowed proxy channel with schema:%v", allowedSchema())
	for _, conf := range GConf.Channel {
		if !conf.Enable {
			continue
		}
		channel := newProxyChannel()
		channel.Conf = conf
		success := false

		for _, server := range conf.ServerList {
			u, err := url.Parse(server)
			if nil != err {
				log.Printf("Invalid server url:%s with reason:%v", server, err)
				continue
			}
			schema := strings.ToLower(u.Scheme)
			if t, ok := proxyTypeTable[schema]; !ok {
				log.Printf("[ERROR]No registe proxy for schema:%s", schema)
				continue
			} else {
				v := reflect.New(t)
				p := v.Interface().(Proxy)
				for i := 0; i < conf.ConnsPerServer; i++ {
					_, err := channel.createMuxSessionByProxy(p, server)
					if nil != err {
						log.Printf("[ERROR]Failed to create mux session for %s:%d with reason:%v", server, i, err)
						break
					} else {
						success = true
					}
				}
			}
		}
		if success {
			log.Printf("Proxy channel:%s init success", conf.Name)
			proxyChannelTable[conf.Name] = channel
		} else {
			log.Printf("Proxy channel:%s init failed", conf.Name)
		}
	}

	//add direct channel
	channel := newProxyChannel()
	channel.Conf.Name = directProxyChannelName
	channel.sessions = make(map[*muxSessionHolder]bool)

	log.Printf("Starting GSnova %s.", local.Version)
	go initDNS()
	go startAdminServer()
	startLocalServers()
	return nil
}

func Stop() error {
	stopLocalServers()
	proxyChannelMutex.Lock()
	for _, pch := range proxyChannelTable {
		for holder := range pch.sessions {
			if nil != holder {
				holder.muxSession.Close()
			}
		}
	}
	proxyChannelTable = make(map[string]*proxyChannel)
	proxyChannelMutex.Unlock()
	hosts.Clear()
	if nil != cnIPRange {
		cnIPRange.Clear()
	}
	return nil
}

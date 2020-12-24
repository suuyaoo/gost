package main

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ginuerzh/gost"
	"github.com/go-log/log"
	"github.com/kardianos/service"
)

var (
	options struct {
		ChainNodes, ServeNodes stringList
		debugMode              bool
	}
	serviceFlag         string
	logger              service.Logger
	clientServiceConfig = &service.Config{
		Name:        "GostClient",
		DisplayName: "Gost Client Service",
		Description: "Start a gost client service.",
		Arguments:   []string{"-C", "client.config"},
	}
)

type program struct{}

func (p *program) Start(s service.Service) error {
	// Start should not block. Do the actual work async.
	go p.run()
	logger.Info("Service start.")
	return nil
}

func (p *program) Stop(s service.Service) error {
	// Stop should not block. Return with a few seconds.
	logger.Info("Service stop.")
	return nil
}

func (p *program) run() {
	logger.Info("Service run.")
	// Do work here
	run()
}

func execPath() (string, error) {
	file, err := exec.LookPath(os.Args[0])
	if err != nil {
		return "", err
	}
	re, err := filepath.Abs(file)
	if err == nil {
		re = filepath.Dir(re)
	}
	return re, err
}

func init() {
	var (
		configureFile string
		printVersion  bool
	)

	flag.StringVar(&serviceFlag, "S", "", "service control, eg: install,uninstall,start,stop")
	flag.Var(&options.ChainNodes, "F", "forward address, can make a forward chain")
	flag.Var(&options.ServeNodes, "L", "listen address, can listen on multiple ports")
	flag.StringVar(&configureFile, "C", "", "configure file")
	flag.BoolVar(&options.debugMode, "D", false, "enable debug log")
	flag.BoolVar(&printVersion, "V", false, "print version")
	flag.Parse()

	if filepath.IsAbs(configureFile) == false {
		execPath, err := execPath()
		if err == nil {
			configureFile = filepath.Join(execPath, configureFile)
		}
	}
	if err := loadConfigureFile(configureFile); err != nil {
		log.Log(err)
		os.Exit(1)
	}

	if flag.NFlag() == 0 {
		flag.PrintDefaults()
		os.Exit(0)
	}

	if printVersion {
		fmt.Fprintf(os.Stderr, "gost %s (%s)\n", gost.Version, runtime.Version())
		os.Exit(0)
	}

	gost.Debug = options.debugMode
}

func run() {
	chain, err := initChain()
	if err != nil {
		log.Log(err)
		os.Exit(1)
	}
	if err := serve(chain); err != nil {
		log.Log(err)
		os.Exit(1)
	}
	select {}
}

func main() {
	prog := &program{}
	s, err := service.New(prog, clientServiceConfig)
	if err != nil {
		log.Log(err)
	}
	logger, err = s.Logger(nil)
	if err != nil {
		log.Log(err)
	}
	if serviceFlag != "" {
		err = service.Control(s, serviceFlag)
		if err != nil {
			log.Log(err)
		}
		fmt.Printf("%s success.\n", serviceFlag)
	} else {
		logger.Info("Starting service...")
		err = s.Run()
		if err != nil {
			logger.Error(err)
		}
		logger.Info("End service...")
	}
}

func initChain() (*gost.Chain, error) {
	chain := gost.NewChain()
	for _, ns := range options.ChainNodes {
		node, err := gost.ParseNode(ns)
		if err != nil {
			return nil, err
		}
		users, err := parseUsers(node.Values.Get("secrets"))
		if err != nil {
			return nil, err
		}
		if node.User == nil && len(users) > 0 {
			node.User = users[0]
		}
		serverName, _, _ := net.SplitHostPort(node.Addr)
		if serverName == "" {
			serverName = "localhost" // default server name
		}

		rootCAs, err := loadCA(node.Values.Get("ca"))
		if err != nil {
			return nil, err
		}
		tlsCfg := &tls.Config{
			ServerName:         serverName,
			InsecureSkipVerify: !toBool(node.Values.Get("scure")),
			RootCAs:            rootCAs,
		}
		var tr gost.Transporter
		switch node.Transport {
		case "tls":
			tr = gost.TLSTransporter()
		case "ws":
			wsOpts := &gost.WSOptions{}
			wsOpts.EnableCompression = toBool(node.Values.Get("compression"))
			wsOpts.ReadBufferSize, _ = strconv.Atoi(node.Values.Get("rbuf"))
			wsOpts.WriteBufferSize, _ = strconv.Atoi(node.Values.Get("wbuf"))
			node.HandshakeOptions = append(node.HandshakeOptions,
				gost.WSOptionsHandshakeOption(wsOpts),
			)
			tr = gost.WSTransporter(nil)
		case "wss":
			tr = gost.WSSTransporter(nil)
		case "kcp":
			if !chain.IsEmpty() {
				return nil, errors.New("KCP must be the first node in the proxy chain")
			}
			config, err := parseKCPConfig(node.Values.Get("c"))
			if err != nil {
				log.Log("[kcp]", err)
			}
			node.HandshakeOptions = append(node.HandshakeOptions,
				gost.KCPConfigHandshakeOption(config),
			)
			tr = gost.KCPTransporter(nil)
		case "ssh":
			if node.Protocol == "direct" || node.Protocol == "remote" || node.Protocol == "forward" {
				tr = gost.SSHForwardTransporter()
			} else {
				tr = gost.SSHTunnelTransporter()
			}

			node.DialOptions = append(node.DialOptions,
				gost.ChainDialOption(chain),
			)
			chain = gost.NewChain() // cutoff the chain for multiplex
		case "quic":
			if !chain.IsEmpty() {
				return nil, errors.New("QUIC must be the first node in the proxy chain")
			}
			config := &gost.QUICConfig{
				TLSConfig: tlsCfg,
				KeepAlive: toBool(node.Values.Get("keepalive")),
			}
			node.HandshakeOptions = append(node.HandshakeOptions,
				gost.QUICConfigHandshakeOption(config),
			)
			tr = gost.QUICTransporter(nil)
		case "http2":
			tr = gost.HTTP2Transporter(nil)
			node.DialOptions = append(node.DialOptions,
				gost.ChainDialOption(chain),
			)
			chain = gost.NewChain() // cutoff the chain for multiplex
		case "h2":
			tr = gost.H2Transporter(nil)
			node.DialOptions = append(node.DialOptions,
				gost.ChainDialOption(chain),
			)
			chain = gost.NewChain() // cutoff the chain for multiplex
		case "h2c":
			tr = gost.H2CTransporter()
			node.DialOptions = append(node.DialOptions,
				gost.ChainDialOption(chain),
			)
			chain = gost.NewChain() // cutoff the chain for multiplex
		case "obfs4":
			if err := gost.Obfs4Init(node, false); err != nil {
				return nil, err
			}
			tr = gost.Obfs4Transporter()
		default:
			tr = gost.TCPTransporter()
		}

		var connector gost.Connector
		switch node.Protocol {
		case "http2":
			connector = gost.HTTP2Connector(node.User)
		case "socks", "socks5":
			connector = gost.SOCKS5Connector(node.User)
		case "socks4":
			connector = gost.SOCKS4Connector()
		case "socks4a":
			connector = gost.SOCKS4AConnector()
		case "ss":
			connector = gost.ShadowConnector(node.User)
		case "direct", "forward":
			connector = gost.SSHDirectForwardConnector()
		case "remote":
			connector = gost.SSHRemoteForwardConnector()
		case "http":
			fallthrough
		default:
			node.Protocol = "http" // default protocol is HTTP
			connector = gost.HTTPConnector(node.User)
		}

		timeout, _ := strconv.Atoi(node.Values.Get("timeout"))
		node.DialOptions = append(node.DialOptions,
			gost.TimeoutDialOption(time.Duration(timeout)*time.Second),
		)

		interval, _ := strconv.Atoi(node.Values.Get("ping"))
		node.HandshakeOptions = append(node.HandshakeOptions,
			gost.AddrHandshakeOption(node.Addr),
			gost.UserHandshakeOption(node.User),
			gost.TLSConfigHandshakeOption(tlsCfg),
			gost.IntervalHandshakeOption(time.Duration(interval)*time.Second),
		)
		node.Client = &gost.Client{
			Connector:   connector,
			Transporter: tr,
		}
		chain.AddNode(node)
	}

	return chain, nil
}

func serve(chain *gost.Chain) error {
	for _, ns := range options.ServeNodes {
		node, err := gost.ParseNode(ns)
		if err != nil {
			return err
		}
		users, err := parseUsers(node.Values.Get("secrets"))
		if err != nil {
			return err
		}
		if node.User != nil {
			users = append(users, node.User)
		}
		tlsCfg, _ := tlsConfig(node.Values.Get("cert"), node.Values.Get("key"))

		var ln gost.Listener
		switch node.Transport {
		case "tls":
			ln, err = gost.TLSListener(node.Addr, tlsCfg)
		case "ws":
			wsOpts := &gost.WSOptions{}
			wsOpts.EnableCompression = toBool(node.Values.Get("compression"))
			wsOpts.ReadBufferSize, _ = strconv.Atoi(node.Values.Get("rbuf"))
			wsOpts.WriteBufferSize, _ = strconv.Atoi(node.Values.Get("wbuf"))
			ln, err = gost.WSListener(node.Addr, wsOpts)
		case "wss":
			wsOpts := &gost.WSOptions{}
			wsOpts.EnableCompression = toBool(node.Values.Get("compression"))
			wsOpts.ReadBufferSize, _ = strconv.Atoi(node.Values.Get("rbuf"))
			wsOpts.WriteBufferSize, _ = strconv.Atoi(node.Values.Get("wbuf"))
			ln, err = gost.WSSListener(node.Addr, tlsCfg, wsOpts)
		case "kcp":
			config, err := parseKCPConfig(node.Values.Get("c"))
			if err != nil {
				log.Log("[kcp]", err)
			}
			ln, err = gost.KCPListener(node.Addr, config)
		case "ssh":
			config := &gost.SSHConfig{
				Users:     users,
				TLSConfig: tlsCfg,
			}
			if node.Protocol == "forward" {
				ln, err = gost.TCPListener(node.Addr)
			} else {
				ln, err = gost.SSHTunnelListener(node.Addr, config)
			}
		case "quic":
			config := &gost.QUICConfig{
				TLSConfig: tlsCfg,
				KeepAlive: toBool(node.Values.Get("keepalive")),
			}
			timeout, _ := strconv.Atoi(node.Values.Get("timeout"))
			config.Timeout = time.Duration(timeout) * time.Second
			ln, err = gost.QUICListener(node.Addr, config)
		case "http2":
			ln, err = gost.HTTP2Listener(node.Addr, tlsCfg)
		case "h2":
			ln, err = gost.H2Listener(node.Addr, tlsCfg)
		case "h2c":
			ln, err = gost.H2CListener(node.Addr)
		case "obfs4":
			if err = gost.Obfs4Init(node, true); err != nil {
				return err
			}
			ln, err = gost.Obfs4Listener(node.Addr)
		case "tcp":
			ln, err = gost.TCPListener(node.Addr)
		case "rtcp":
			if chain.LastNode().Protocol == "forward" && chain.LastNode().Transport == "ssh" {
				chain.Nodes()[len(chain.Nodes())-1].Client.Connector = gost.SSHRemoteForwardConnector()
			}
			ln, err = gost.TCPRemoteForwardListener(node.Addr, chain)
		case "udp":
			ttl, _ := strconv.Atoi(node.Values.Get("ttl"))
			ln, err = gost.UDPDirectForwardListener(node.Addr, time.Duration(ttl)*time.Second)
		case "rudp":
			ttl, _ := strconv.Atoi(node.Values.Get("ttl"))
			ln, err = gost.UDPRemoteForwardListener(node.Addr, chain, time.Duration(ttl)*time.Second)
		case "redirect":
			ln, err = gost.TCPListener(node.Addr)
		case "ssu":
			ttl, _ := strconv.Atoi(node.Values.Get("ttl"))
			ln, err = gost.ShadowUDPListener(node.Addr, node.User, time.Duration(ttl)*time.Second)
		default:
			ln, err = gost.TCPListener(node.Addr)
		}
		if err != nil {
			return err
		}

		var whitelist, blacklist *gost.Permissions
		if node.Values.Get("whitelist") != "" {
			if whitelist, err = gost.ParsePermissions(node.Values.Get("whitelist")); err != nil {
				return err
			}
		} else {
			// By default allow for everyting
			whitelist, _ = gost.ParsePermissions("*:*:*")
		}

		if node.Values.Get("blacklist") != "" {
			if blacklist, err = gost.ParsePermissions(node.Values.Get("blacklist")); err != nil {
				return err
			}
		} else {
			// By default block nothing
			blacklist, _ = gost.ParsePermissions("")
		}

		var handlerOptions []gost.HandlerOption

		handlerOptions = append(handlerOptions,
			gost.AddrHandlerOption(node.Addr),
			gost.ChainHandlerOption(chain),
			gost.UsersHandlerOption(users...),
			gost.TLSConfigHandlerOption(tlsCfg),
			gost.WhitelistHandlerOption(whitelist),
			gost.BlacklistHandlerOption(blacklist),
		)
		var handler gost.Handler
		switch node.Protocol {
		case "http2":
			handler = gost.HTTP2Handler(handlerOptions...)
		case "socks", "socks5":
			handler = gost.SOCKS5Handler(handlerOptions...)
		case "socks4", "socks4a":
			handler = gost.SOCKS4Handler(handlerOptions...)
		case "ss":
			handler = gost.ShadowHandler(handlerOptions...)
		case "http":
			handler = gost.HTTPHandler(handlerOptions...)
		case "tcp":
			handler = gost.TCPDirectForwardHandler(node.Remote, handlerOptions...)
		case "rtcp":
			handler = gost.TCPRemoteForwardHandler(node.Remote, handlerOptions...)
		case "udp":
			handler = gost.UDPDirectForwardHandler(node.Remote, handlerOptions...)
		case "rudp":
			handler = gost.UDPRemoteForwardHandler(node.Remote, handlerOptions...)
		case "forward":
			handler = gost.SSHForwardHandler(handlerOptions...)
		case "redirect":
			handler = gost.TCPRedirectHandler(handlerOptions...)
		case "ssu":
			handler = gost.ShadowUDPdHandler(handlerOptions...)
		default:
			handler = gost.AutoHandler(handlerOptions...)
		}
		go new(gost.Server).Serve(ln, handler)
	}

	return nil
}

// Load the certificate from cert and key files, will use the default certificate if the provided info are invalid.
func tlsConfig(certFile, keyFile string) (*tls.Config, error) {
	if certFile == "" {
		certFile = "cert.pem"
	}
	if keyFile == "" {
		keyFile = "key.pem"
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

func loadCA(caFile string) (cp *x509.CertPool, err error) {
	if caFile == "" {
		return
	}
	cp = x509.NewCertPool()
	data, err := ioutil.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	if !cp.AppendCertsFromPEM(data) {
		return nil, errors.New("AppendCertsFromPEM failed")
	}
	return
}

func loadConfigureFile(configureFile string) error {
	if configureFile == "" {
		return nil
	}
	content, err := ioutil.ReadFile(configureFile)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(content, &options); err != nil {
		return err
	}
	return nil
}

type stringList []string

func (l *stringList) String() string {
	return fmt.Sprintf("%s", *l)
}
func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

func toBool(s string) bool {
	if b, _ := strconv.ParseBool(s); b {
		return b
	}
	n, _ := strconv.Atoi(s)
	return n > 0
}

func parseKCPConfig(configFile string) (*gost.KCPConfig, error) {
	if configFile == "" {
		return nil, nil
	}
	file, err := os.Open(configFile)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	config := &gost.KCPConfig{}
	if err = json.NewDecoder(file).Decode(config); err != nil {
		return nil, err
	}
	return config, nil
}

func parseUsers(authFile string) (users []*url.Userinfo, err error) {
	if authFile == "" {
		return
	}

	file, err := os.Open(authFile)
	if err != nil {
		return
	}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		s := strings.SplitN(line, " ", 2)
		if len(s) == 1 {
			users = append(users, url.User(strings.TrimSpace(s[0])))
		} else if len(s) == 2 {
			users = append(users, url.UserPassword(strings.TrimSpace(s[0]), strings.TrimSpace(s[1])))
		}
	}

	err = scanner.Err()
	return
}

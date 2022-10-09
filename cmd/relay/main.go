// Relay packets to/from a tunneled interface
// Intended for use with Docker Deskstop on MacOSX which doesn't properly handle this
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/decent-e/dnssd"
)

const (
	SERVER_TYPE = "tcp"
	MAXDGRAMSZ  = 8192
	TIMEFORMAT  = "15:04:05.000"
)

var (
	mode       = flag.String("mode", "server", "mode (client or server")
	debug      = flag.Bool("d", false, "enable debug prints")
	dump       = flag.Bool("dump", false, "dump all traffic")
	serverIp   = flag.String("serverIp", "127.0.0.1", "IP to listen on for new clients")
	relayHost  = flag.String("host", "host.docker.internal", "host the client mode connects to")
	relayPort  = flag.Int("port", 53535, "port connected to for relay traffic")
	relayFPort = flag.Int("from", 35353, "port to send relayed messages from")
	dnssdAddr  = flag.String("dnssdAddr", "224.0.0.251", "multicast address to use for dns-sd")
	dnssdPort  = flag.Int("dnssdPort", 5353, "port to use for mDNS dns-sd")
	v          = func(string, ...interface{}) {}
)

func flags() {
	flag.Parse()
	if *debug {
		v = log.Printf
	}
}

// provide a reliable data-gram service over TCP
type TcpDGC struct {
	connection net.Conn
}

func (c *TcpDGC) readN(msg []byte, total uint32) error {
	totalread := uint32(0)

	for totalread < total {
		n, err := c.connection.Read(msg[totalread:total])
		if err != nil {
			v("readN: short read error")
			return err
		}
		if n < 0 {
			v("n returned from read is negative")
			return fmt.Errorf("negative return in readN")
		}
		totalread += uint32(n)
	}
	return nil
}

func (c *TcpDGC) Send(msg []byte) (int, error) {
	hdr := make([]byte, 4)
	binary.LittleEndian.PutUint32(hdr, uint32(len(msg)))
	actualBuf := append(hdr[:], msg[:]...)
	v("Sending msg size %x", uint32(len(actualBuf)))
	n, err := c.connection.Write(actualBuf)

	return (n - 4), err
}

func (c *TcpDGC) Recv(msg []byte) (uint32, error) {
	hdr := make([]byte, 4)
	err := c.readN(hdr, 4)
	if err != nil {
		return 0, err
	}

	buflen := binary.LittleEndian.Uint32(hdr)
	if buflen > uint32(len(msg)) {
		return 0, fmt.Errorf("buffer not big enough to store datagram size %x", buflen)
	}
	v("receiving message size %x", buflen)
	err = c.readN(msg, buflen)
	if err != nil {
		return 0, err
	}

	return buflen, err
}

func relay(connection net.Conn, tunChan chan *dnssd.Request, ctlChan chan int) {
	c := &TcpDGC{connection}

	for {
		select {
		case <-ctlChan:
			v("Received request to stop in relay")
			os.Exit(1)
		case r := <-tunChan:
			if r.From().Port == *relayFPort {
				// Drop multicasts from me
				continue
			}
			m := r.Raw()
			rm, err := m.Pack()
			if err != nil {
				log.Printf("repacking DNS msg: %s\n", err.Error())
				return
			}

			n, err := c.Send(rm)
			if err != nil {
				log.Printf("Forwarding repacked DNS msg: %s\n", err.Error())
				return
			}
			if n < len(rm) {
				log.Printf("Forwarding repacked DNS msg returned partial send of %d/%d bytes\n", n, len(rm))
				return
			}
		}
	}
}

func relayToPeer(connection net.Conn, stop chan os.Signal) {
	tunChan := make(chan *dnssd.Request, 64)
	ctlChan := make(chan int, 1)

	v("Relaying…\n")

	go relay(connection, tunChan, ctlChan)

	fn := func(req *dnssd.Request) {
		tunChan <- req
		if *dump {
			log.Printf("-------------------------------------------\n")
			log.Printf("%s\n%v\n", time.Now().Format(TIMEFORMAT), req)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if rsp, err := dnssd.NewResponder(); err != nil {
		log.Println(err)
	} else {
		rsp.Debug(ctx, fn)

		<-stop

		v("Interrupt detected")
		cancel()
		ctlChan <- 1
		os.Exit(1)
	}
}

func server(stop chan os.Signal) {
	v("Server Running...")

	server, err := net.Listen(SERVER_TYPE, *serverIp+":"+strconv.Itoa(*relayPort))
	if err != nil {
		log.Println("error listening:", err.Error())
		os.Exit(1)
	}
	defer server.Close()
	v("Listening on " + *serverIp + ":" + strconv.Itoa(*relayPort))
	v("Waiting for client...")
	go func() {
		<-stop
		server.Close()
		os.Exit(1)
	}()
	for {
		connection, err := server.Accept()
		if err != nil {
			log.Printf("error accepting: %s\n", err.Error())
			os.Exit(1)
		}
		log.Printf("new client connected %s\n", connection.RemoteAddr().String())
		go relayToPeer(connection, stop)
		go relayFromPeer(connection)
	}
}

func relayFromPeer(connection net.Conn) {
	maddr, err := net.ResolveUDPAddr("udp", *dnssdAddr+":"+strconv.Itoa(*dnssdPort))
	if err != nil {
		log.Fatal(err)
	}
	myaddr, err := net.ResolveUDPAddr("udp", ":"+strconv.Itoa(*relayFPort))
	if err != nil {
		log.Fatal(err)
	}
	mc, err := net.DialUDP("udp", myaddr, maddr)
	if err != nil {
		log.Fatal(err)
	}
	defer mc.Close()

	c := &TcpDGC{connection}
	for {
		buffer := make([]byte, MAXDGRAMSZ)

		mLen, err := c.Recv(buffer)
		if err != nil {
			log.Printf("error reading from %s: %s\n", connection.RemoteAddr().String(), err.Error())
			return
		}

		n, err := mc.Write(buffer[:mLen])
		if err != nil {
			log.Printf("error forwarding message as %s: %s\n", mc.LocalAddr().String(), err.Error())
			return
		}
		if uint32(n) < mLen {
			log.Printf("short write as %s: %d\n", mc.LocalAddr().String(), n)
		}
	}
}

func client(stop chan os.Signal) {
	connection, err := net.Dial(SERVER_TYPE, *relayHost+":"+strconv.Itoa(*relayPort))
	if err != nil {
		log.Fatal(err)
	}
	defer connection.Close()
	go func() {
		<-stop
		connection.Close()
		os.Exit(1)
	}()

	go relayToPeer(connection, stop)
	relayFromPeer(connection)
}

func main() {
	flags()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)

	if *mode == "server" {
		v("starting server")
		server(stop)
	}
	if *mode == "client" {
		v("starting client")
		client(stop)
	}
}

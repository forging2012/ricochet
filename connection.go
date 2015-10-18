// connection.go - Ricochet connection implementation.
//
// To the extent possible under law, Yawning Angel waived all copyright
// and related or neighboring rights to ricochet (library), using the Creative
// Commons "CC0" public domain dedication. See LICENSE or
// <http://creativecommons.org/publicdomain/zero/1.0/> for full details.

package ricochet

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"sync"
	"time"

	"golang.org/x/net/proxy"

	"github.com/yawning/ricochet/packet"
)

const (
	protocolVersion      = 0x01
	protocolVersionError = 0xff

	packetHdrSize = 2 + 2

	handshakeTimeout      = 30 * time.Second
	authenticationTimeout = 30 * time.Second
)

var handshakePrefix = []byte{0x49, 0x4d}

type ricochetConn struct {
	sync.Mutex

	endpoint *Endpoint
	conn     net.Conn
	hostname string

	chanMap   map[uint16]ricochetChan
	authTimer *time.Timer

	isServer   bool
	nextChanID uint16
}

type ricochetChan interface {
	onOpenChannel() error
	onChannelResult(*packet.ChannelResult) error
	onPacket([]byte) error
	onClose() error
}

func (c *ricochetConn) nextPacket() (uint16, []byte, error) {
	// Read a packet from the network connection.
	//  uint16_t size (Including header)
	//  uint16_t channel
	//  uint16_t data
	var pktHdr [4]byte
	if _, err := io.ReadFull(c.conn, pktHdr[:]); err != nil {
		return 0, nil, fmt.Errorf("failed to read pkt header: %v", err)
	}
	pktSize := binary.BigEndian.Uint16(pktHdr[0:])
	pktChan := binary.BigEndian.Uint16(pktHdr[2:])
	if pktSize <= packetHdrSize {
		return pktChan, nil, io.EOF
	}
	pktSize -= packetHdrSize
	pktData := make([]byte, pktSize)
	if _, err := io.ReadFull(c.conn, pktData); err != nil {
		return 0, nil, fmt.Errorf("failed to read pkt data: %v", err)
	}
	return pktChan, pktData, nil
}

func (c *ricochetConn) sendPacket(pktChan uint16, pktData []byte) error {
	c.Lock()
	defer c.Unlock()

	pktLen := packetHdrSize + len(pktData)
	if pktLen > math.MaxUint16 {
		return fmt.Errorf("pkt > max size: %v", pktLen)
	}

	var pktHdr [4]byte
	binary.BigEndian.PutUint16(pktHdr[0:], uint16(pktLen))
	binary.BigEndian.PutUint16(pktHdr[2:], pktChan)
	if _, err := c.conn.Write(pktHdr[:]); err != nil {
		return err
	}
	_, err := c.conn.Write(pktData)
	return err
}

func (c *ricochetConn) allocateNextChanID() (uint16, error) {
	c.Lock()
	defer c.Unlock()

	chanID := c.nextChanID
	if ((int)(chanID) + 2) > math.MaxUint16 {
		// Could happen, unlikely to happen since it means that over 32k
		// channels were opened
		//
		// TODO: Handle this in a better manner.
		return 0, fmt.Errorf("bug: next channel ID will wrap")
	}
	c.nextChanID += 2 // Channel IDs are all odd or even.
	return chanID, nil
}

func (c *ricochetConn) clientHandshake(d proxy.Dialer, dialHostname string) {
	var err error
	defer func() {
		if c.conn != nil {
			c.conn.Close()
		}
		c.endpoint.contacts.onOutgoingConnectionClosed(c)
	}()

	// Open the connection to the remote HS.
	c.conn, err = d.Dial("tcp", dialHostname)
	if err != nil {
		log.Printf("client: Failed to connect to '%v' : %v", dialHostname, err)
		return
	}

	// Arm the handshake timeout.
	if err := c.conn.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		log.Printf("client: Failed to arm handshake timeout: %v", err)
		return
	}

	// Send prefix | nVersions | version.
	hsPrefix := append(handshakePrefix, 1)       // Sending one version...
	hsPrefix = append(hsPrefix, protocolVersion) // ... this one.
	if _, err := c.conn.Write(hsPrefix); err != nil {
		log.Printf("client: Failed to send prefix | nVersions | version: %v", err)
		return
	}

	// Read the negotiated version.
	var respVer [1]byte
	if _, err := io.ReadFull(c.conn, respVer[:]); err != nil {
		log.Printf("client: Failed to read negotiated version: %v", err)
		return
	}
	if respVer[0] != protocolVersion {
		log.Printf("client: Server speaks no compatible versions, closing")
		return
	}

	// Disarm the handshake timeout.
	if err := c.conn.SetDeadline(time.Time{}); err != nil {
		log.Printf("client: Failed to disarm handshake timeout: %v", err)
		return
	}

	c.chanMap = make(map[uint16]ricochetChan)
	c.chanMap[controlChanID] = newControlChan(c, controlChanID)

	fuck := func() { _ = c.conn.Close() }
	c.authTimer = time.AfterFunc(authenticationTimeout, fuck)

	// Send the OpenChannel(AuthHS) request before doing anything else.  The
	// rest of the process is driven by receiving responses from the server,
	// or 'fuck()'.
	if err := newClientAuthHSChan(c); err != nil {
		log.Printf("client: Failed to start authentication: %v", err)
		return
	}

	c.incomingPacketWorker()
}

func (c *ricochetConn) serverHandshake() {
	var err error
	defer c.conn.Close()

	log.Printf("server: new client connection")

	// Arm the handshake timeout.
	if err = c.conn.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		log.Printf("server: Failed to arm handshake timeout: %v", err)
		return
	}

	// Read in the protocol versions supported by the client.
	var hsPrefix [3]byte
	if _, err = io.ReadFull(c.conn, hsPrefix[:]); err != nil {
		log.Printf("server: Failed to read in prefix | nVersions: %v", err)
		return
	}
	if !bytes.Equal(hsPrefix[0:2], handshakePrefix) {
		log.Printf("server: Invalid handshake prefix")
		return
	}
	versions := make([]byte, hsPrefix[2])
	if _, err = io.ReadFull(c.conn, versions); err != nil {
		log.Printf("server: Failed to read in versions: %v", err)
		return
	}

	// Determine if the client speaks a supported version.
	respVer := []byte{protocolVersionError}
	for _, v := range versions {
		if v == protocolVersion {
			respVer[0] = protocolVersion
			break
		}
	}
	if _, err = c.conn.Write(respVer); err != nil {
		log.Printf("server: Failed to send protocol version: %v", err)
		return
	}
	if respVer[0] == protocolVersionError {
		log.Printf("server: Client speaks no compatible versions, closing")
		return
	}

	// Disarm the handshake timeout.
	if err = c.conn.SetDeadline(time.Time{}); err != nil {
		log.Printf("server: Failed to disarm handshake timeout: %v", err)
		return
	}

	c.chanMap = make(map[uint16]ricochetChan)
	c.chanMap[controlChanID] = newControlChan(c, controlChanID)

	fuck := func() { _ = c.conn.Close() }
	c.authTimer = time.AfterFunc(authenticationTimeout, fuck)

	c.incomingPacketWorker()

	// XXX: Remove from global state.
}

func (c *ricochetConn) incomingPacketWorker() {
	for {
		chanID, rawPkt, err := c.nextPacket()
		if err == io.EOF {
			if err = c.demuxChanClose(chanID); err == nil {
				continue
			}
		}
		if err != nil {
			log.Printf("worker: early pkt processing: %v", err)
			return
		}

		ch := c.chanMap[chanID]
		if ch == nil {
			log.Printf("worker: received pkt for invalid channel: %v", err)
			return
		}
		if err = ch.onPacket(rawPkt); err != nil {
			log.Printf("worker: channel pkt processing: %v", err)
			return
		}
	}
}

func (c *ricochetConn) demuxChanClose(chanID uint16) error {
	ch := c.chanMap[chanID]
	if ch == nil {
		return fmt.Errorf("close for invalid channel: %v", chanID)
	}
	return ch.onClose()
}
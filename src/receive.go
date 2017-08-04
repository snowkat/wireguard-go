package main

import (
	"bytes"
	"encoding/binary"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type QueueHandshakeElement struct {
	msgType uint32
	packet  []byte
	buffer  *[MaxMessageSize]byte
	source  *net.UDPAddr
}

type QueueInboundElement struct {
	dropped int32
	mutex   sync.Mutex
	buffer  *[MaxMessageSize]byte
	packet  []byte
	counter uint64
	keyPair *KeyPair
}

func (elem *QueueInboundElement) Drop() {
	atomic.StoreInt32(&elem.dropped, AtomicTrue)
}

func (elem *QueueInboundElement) IsDropped() bool {
	return atomic.LoadInt32(&elem.dropped) == AtomicTrue
}

func (device *Device) addToInboundQueue(
	queue chan *QueueInboundElement,
	element *QueueInboundElement,
) {
	for {
		select {
		case queue <- element:
			return
		default:
			select {
			case old := <-queue:
				old.Drop()
			default:
			}
		}
	}
}

func (device *Device) addToHandshakeQueue(
	queue chan QueueHandshakeElement,
	element QueueHandshakeElement,
) {
	for {
		select {
		case queue <- element:
			return
		default:
			select {
			case elem := <-queue:
				device.PutMessageBuffer(elem.buffer)
			default:
			}
		}
	}
}

/* Routine determining the busy state of the interface
 *
 * TODO: Under load for some time
 */
func (device *Device) RoutineBusyMonitor() {
	samples := 0
	interval := time.Second
	for timer := time.NewTimer(interval); ; {

		select {
		case <-device.signal.stop:
			return
		case <-timer.C:
		}

		// compute busy heuristic

		if len(device.queue.handshake) > QueueHandshakeBusySize {
			samples += 1
		} else if samples > 0 {
			samples -= 1
		}
		samples %= 30
		busy := samples > 5

		// update busy state

		if busy {
			atomic.StoreInt32(&device.underLoad, AtomicTrue)
		} else {
			atomic.StoreInt32(&device.underLoad, AtomicFalse)
		}

		timer.Reset(interval)
	}
}

func (device *Device) RoutineReceiveIncomming() {

	logInfo := device.log.Info
	logDebug := device.log.Debug
	logDebug.Println("Routine, receive incomming, started")

	var buffer *[MaxMessageSize]byte

	for {

		// check if stopped

		select {
		case <-device.signal.stop:
			return
		default:
		}

		// read next datagram

		if buffer == nil {
			buffer = device.GetMessageBuffer()
		}

		// TODO: Take writelock to sleep
		device.net.mutex.RLock()
		conn := device.net.conn
		device.net.mutex.RUnlock()
		if conn == nil {
			time.Sleep(time.Second)
			continue
		}

		// TODO: Wait for new conn or message
		conn.SetReadDeadline(time.Now().Add(time.Second))

		size, raddr, err := conn.ReadFromUDP(buffer[:])
		if err != nil || size < MinMessageSize {
			continue
		}

		// handle packet

		packet := buffer[:size]
		msgType := binary.LittleEndian.Uint32(packet[:4])

		func() {
			switch msgType {

			case MessageInitiationType, MessageResponseType:

				// TODO: Check size early

				// add to handshake queue

				device.addToHandshakeQueue(
					device.queue.handshake,
					QueueHandshakeElement{
						msgType: msgType,
						buffer:  buffer,
						packet:  packet,
						source:  raddr,
					},
				)
				buffer = nil

			case MessageCookieReplyType:

				// TODO: Queue all the things

				// verify and update peer cookie state

				if len(packet) != MessageCookieReplySize {
					return
				}

				var reply MessageCookieReply
				reader := bytes.NewReader(packet)
				err := binary.Read(reader, binary.LittleEndian, &reply)
				if err != nil {
					logDebug.Println("Failed to decode cookie reply")
					return
				}
				device.ConsumeMessageCookieReply(&reply)

			case MessageTransportType:

				// lookup key pair

				if len(packet) < MessageTransportSize {
					return
				}

				receiver := binary.LittleEndian.Uint32(
					packet[MessageTransportOffsetReceiver:MessageTransportOffsetCounter],
				)
				value := device.indices.Lookup(receiver)
				keyPair := value.keyPair
				if keyPair == nil {
					return
				}

				// check key-pair expiry

				if keyPair.created.Add(RejectAfterTime).Before(time.Now()) {
					return
				}

				// add to peer queue

				peer := value.peer
				elem := &QueueInboundElement{
					packet:  packet,
					buffer:  buffer,
					keyPair: keyPair,
					dropped: AtomicFalse,
				}
				elem.mutex.Lock()

				// add to decryption queues

				device.addToInboundQueue(device.queue.decryption, elem)
				device.addToInboundQueue(peer.queue.inbound, elem)
				buffer = nil

			default:
				logInfo.Println("Got unknown message from:", raddr)
			}
		}()
	}
}

func (device *Device) RoutineDecryption() {
	var elem *QueueInboundElement
	var nonce [chacha20poly1305.NonceSize]byte

	logDebug := device.log.Debug
	logDebug.Println("Routine, decryption, started for device")

	for {
		select {
		case elem = <-device.queue.decryption:
		case <-device.signal.stop:
			return
		}

		// check if dropped

		if elem.IsDropped() {
			elem.mutex.Unlock() // TODO: Make consistent with send
			continue
		}

		// split message into fields

		counter := elem.packet[MessageTransportOffsetCounter:MessageTransportOffsetContent]
		content := elem.packet[MessageTransportOffsetContent:]

		// decrypt with key-pair

		var err error
		copy(nonce[4:], counter)
		elem.counter = binary.LittleEndian.Uint64(counter)
		elem.packet, err = elem.keyPair.receive.Open(
			elem.buffer[:0],
			nonce[:],
			content,
			nil,
		)
		if err != nil {
			elem.Drop()
		}
		elem.mutex.Unlock()
	}
}

/* Handles incomming packets related to handshake
 *
 *
 */
func (device *Device) RoutineHandshake() {

	logInfo := device.log.Info
	logError := device.log.Error
	logDebug := device.log.Debug
	logDebug.Println("Routine, handshake routine, started for device")

	var temp [256]byte
	var elem QueueHandshakeElement

	for {
		select {
		case elem = <-device.queue.handshake:
		case <-device.signal.stop:
			return
		}

		func() {

			// verify mac1

			if !device.mac.CheckMAC1(elem.packet) {
				logDebug.Println("Received packet with invalid mac1")
				return
			}

			// verify mac2

			busy := atomic.LoadInt32(&device.underLoad) == AtomicTrue

			if busy && !device.mac.CheckMAC2(elem.packet, elem.source) {
				sender := binary.LittleEndian.Uint32(elem.packet[4:8]) // "sender" always follows "type"
				reply, err := device.CreateMessageCookieReply(elem.packet, sender, elem.source)
				if err != nil {
					logError.Println("Failed to create cookie reply:", err)
					return
				}
				// TODO: Use temp
				writer := bytes.NewBuffer(elem.packet[:0])
				binary.Write(writer, binary.LittleEndian, reply)
				elem.packet = writer.Bytes()
				_, err = device.net.conn.WriteToUDP(elem.packet, elem.source)
				if err != nil {
					logDebug.Println("Failed to send cookie reply:", err)
				}
				return
			}

			// ratelimit

			// TODO: Only ratelimit when busy

			if !device.ratelimiter.Allow(elem.source.IP) {
				return
			}

			// handle messages

			switch elem.msgType {
			case MessageInitiationType:

				// unmarshal

				if len(elem.packet) != MessageInitiationSize {
					return
				}

				var msg MessageInitiation
				reader := bytes.NewReader(elem.packet)
				err := binary.Read(reader, binary.LittleEndian, &msg)
				if err != nil {
					logError.Println("Failed to decode initiation message")
					return
				}

				// consume initiation

				peer := device.ConsumeMessageInitiation(&msg)
				if peer == nil {
					logInfo.Println(
						"Recieved invalid initiation message from",
						elem.source.IP.String(),
						elem.source.Port,
					)
					return
				}

				// update timers

				peer.TimerAnyAuthenticatedPacketTraversal()
				peer.TimerAnyAuthenticatedPacketReceived()

				// update endpoint
				// TODO: Add a race condition \s

				peer.mutex.Lock()
				peer.endpoint = elem.source
				peer.mutex.Unlock()

				// create response

				response, err := device.CreateMessageResponse(peer)
				if err != nil {
					logError.Println("Failed to create response message:", err)
					return
				}

				peer.TimerEphemeralKeyCreated()
				peer.NewKeyPair()

				logDebug.Println("Creating response message for", peer.String())

				writer := bytes.NewBuffer(temp[:0])
				binary.Write(writer, binary.LittleEndian, response)
				packet := writer.Bytes()
				peer.mac.AddMacs(packet)

				// send response

				peer.SendBuffer(packet)
				peer.TimerAnyAuthenticatedPacketTraversal()

			case MessageResponseType:

				// unmarshal

				if len(elem.packet) != MessageResponseSize {
					return
				}

				var msg MessageResponse
				reader := bytes.NewReader(elem.packet)
				err := binary.Read(reader, binary.LittleEndian, &msg)
				if err != nil {
					logError.Println("Failed to decode response message")
					return
				}

				// consume response

				peer := device.ConsumeMessageResponse(&msg)
				if peer == nil {
					logInfo.Println(
						"Recieved invalid response message from",
						elem.source.IP.String(),
						elem.source.Port,
					)
					return
				}

				// update timers

				peer.TimerAnyAuthenticatedPacketTraversal()
				peer.TimerAnyAuthenticatedPacketReceived()
				peer.TimerHandshakeComplete()

				// derive key-pair

				peer.NewKeyPair()
				peer.SendKeepAlive()

			default:
				logError.Println("Invalid message type in handshake queue")
			}
		}()
	}
}

func (peer *Peer) RoutineSequentialReceiver() {
	var elem *QueueInboundElement

	device := peer.device

	logInfo := device.log.Info
	logDebug := device.log.Debug
	logDebug.Println("Routine, sequential receiver, started for peer", peer.id)

	for {
		// wait for decryption

		select {
		case <-peer.signal.stop:
			return
		case elem = <-peer.queue.inbound:
		}
		elem.mutex.Lock()

		// process packet

		func() {
			if elem.IsDropped() {
				return
			}

			// check for replay

			if !elem.keyPair.replayFilter.ValidateCounter(elem.counter) {
				return
			}

			peer.TimerAnyAuthenticatedPacketTraversal()
			peer.TimerAnyAuthenticatedPacketReceived()
			peer.KeepKeyFreshReceiving()

			// check if using new key-pair

			kp := &peer.keyPairs
			kp.mutex.Lock()
			if kp.next == elem.keyPair {
				peer.TimerHandshakeComplete()
				kp.previous = kp.current
				kp.current = kp.next
				kp.next = nil
			}
			kp.mutex.Unlock()

			// check for keep-alive

			if len(elem.packet) == 0 {
				logDebug.Println("Received keep-alive from", peer.String())
				return
			}
			peer.TimerDataReceived()

			// verify source and strip padding

			switch elem.packet[0] >> 4 {
			case ipv4.Version:

				// strip padding

				if len(elem.packet) < ipv4.HeaderLen {
					return
				}

				field := elem.packet[IPv4offsetTotalLength : IPv4offsetTotalLength+2]
				length := binary.BigEndian.Uint16(field)
				// TODO: check length of packet & NOT TOO SMALL either
				elem.packet = elem.packet[:length]

				// verify IPv4 source

				src := elem.packet[IPv4offsetSrc : IPv4offsetSrc+net.IPv4len]
				if device.routingTable.LookupIPv4(src) != peer {
					logInfo.Println("Packet with unallowed source IP from", peer.String())
					return
				}

			case ipv6.Version:

				// strip padding

				if len(elem.packet) < ipv6.HeaderLen {
					return
				}

				field := elem.packet[IPv6offsetPayloadLength : IPv6offsetPayloadLength+2]
				length := binary.BigEndian.Uint16(field)
				length += ipv6.HeaderLen
				// TODO: check length of packet
				elem.packet = elem.packet[:length]

				// verify IPv6 source

				src := elem.packet[IPv6offsetSrc : IPv6offsetSrc+net.IPv6len]
				if device.routingTable.LookupIPv6(src) != peer {
					logInfo.Println("Packet with unallowed source IP from", peer.String())
					return
				}

			default:
				logInfo.Println("Packet with invalid IP version from", peer.String())
				return
			}

			atomic.AddUint64(&peer.stats.rxBytes, uint64(len(elem.packet)))
			device.addToInboundQueue(device.queue.inbound, elem)

			// TODO: move TUN write into per peer routine
		}()
	}
}

func (device *Device) RoutineWriteToTUN() {

	logError := device.log.Error
	logDebug := device.log.Debug
	logDebug.Println("Routine, sequential tun writer, started")

	for {
		select {
		case <-device.signal.stop:
			return
		case elem := <-device.queue.inbound:
			_, err := device.tun.Write(elem.packet)
			device.PutMessageBuffer(elem.buffer)
			if err != nil {
				logError.Println("Failed to write packet to TUN device:", err)
			}
		}
	}
}

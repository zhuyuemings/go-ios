package instruments

import (
	"strings"
	"sync"

	"github.com/danielpaulus/go-ios/ios"
	dtx "github.com/danielpaulus/go-ios/ios/dtx_codec"
	log "github.com/sirupsen/logrus"
)

const (
	NetworkMonitorIdentifier = "com.apple.instruments.server.services.networking"
)

type networkMonitorMsgDispatcher struct {
	messages chan dtx.Message
	conn     *dtx.Connection
}

func (p *networkMonitorMsgDispatcher) Dispatch(m dtx.Message) {
	if p.conn != nil {
		dtx.SendAckIfNeeded(p.conn, m)
	}
	p.messages <- m
}

type NetworkMonitorService struct {
	channel       *dtx.Channel
	conn          *dtx.Connection
	msgDispatcher *networkMonitorMsgDispatcher
	aggregator    *NetworkMonitorAggregator
}

type NetworkMonitorAggregator struct {
	mu          sync.Mutex
	interfaces  map[uint64]string          // interface_index -> name
	connections map[uint64]*ConnectionInfo // connection_serial -> ConnectionInfo
	pidBytesMap map[uint64]*NetworkBytes   // pid -> {rx, tx}
	globalBytes *NetworkBytes              // global {rx, tx}
}

type ConnectionInfo struct {
	PID            uint64
	InterfaceIndex uint64
	IsValid        bool // Whether this connection is on an allowed interface
	LastRxBytes    uint64
	LastTxBytes    uint64
}

type NetworkBytes struct {
	RxBytes uint64
	TxBytes uint64
}

func NewNetworkMonitorService(device ios.DeviceEntry) (*NetworkMonitorService, error) {
	dispatcher := &networkMonitorMsgDispatcher{messages: make(chan dtx.Message, 1000)}
	
	dtxConn, err := connectInstrumentsWithMsgDispatcher(device, dispatcher)
	if err != nil {
		return nil, err
	}
	dispatcher.conn = dtxConn

	channel := dtxConn.RequestChannelIdentifier(NetworkMonitorIdentifier, dispatcher)
	
	aggregator := &NetworkMonitorAggregator{
		interfaces:  make(map[uint64]string),
		connections: make(map[uint64]*ConnectionInfo),
		pidBytesMap: make(map[uint64]*NetworkBytes),
		globalBytes: &NetworkBytes{},
	}

	service := &NetworkMonitorService{
		channel:       channel,
		conn:          dtxConn,
		msgDispatcher: dispatcher,
		aggregator:    aggregator,
	}

	go service.listenForEvents()

	return service, nil
}

func (s *NetworkMonitorService) StartMonitoring() error {
	_, err := s.channel.MethodCall("startMonitoring")
	return err
}

func (s *NetworkMonitorService) StopMonitoring() error {
	_, err := s.channel.MethodCall("stopMonitoring")
	return err
}

func (s *NetworkMonitorService) Close() error {
	s.StopMonitoring()
	close(s.msgDispatcher.messages)
	return nil
}

func (s *NetworkMonitorService) GetAggregator() *NetworkMonitorAggregator {
	return s.aggregator
}

func (s *NetworkMonitorService) listenForEvents() {
	for msg := range s.msgDispatcher.messages {
		if len(msg.Payload) < 2 {
			continue
		}

		eventType, ok := msg.Payload[0].(int) // DTX sometimes parses small numbers as int
		if !ok {
			// Try uint64 fallback
			if et, ok2 := ToUint64(msg.Payload[0]); ok2 {
				eventType = int(et)
			} else {
				continue
			}
		}

		args, ok := msg.Payload[1].([]interface{})
		if !ok {
			continue
		}

		switch eventType {
		case 0: // InterfaceDetectionEvent
			if len(args) >= 2 {
				idx, valid1 := ToUint64(args[0])
				name, valid2 := args[1].(string)
				if valid1 && valid2 {
					s.aggregator.AddInterface(idx, name)
				}
			}
		case 1: // ConnectionDetectionEvent
			if len(args) >= 8 {
				ifaceIdx, _ := ToUint64(args[2])
				pid, _ := ToUint64(args[3])
				serial, _ := ToUint64(args[6])
				s.aggregator.AddConnection(serial, pid, ifaceIdx)
			}
		case 2: // ConnectionUpdateEvent
			if len(args) >= 11 {
				rxBytes, _ := ToUint64(args[1])
				txBytes, _ := ToUint64(args[3])
				serial, _ := ToUint64(args[9])
				s.aggregator.UpdateConnection(serial, rxBytes, txBytes)
			}
		}
	}
}

// isAllowedInterface returns true if the interface name starts with "en" or "pdp_ip"
func isAllowedInterface(name string) bool {
	return strings.HasPrefix(name, "en") || strings.HasPrefix(name, "pdp_ip")
}

func (a *NetworkMonitorAggregator) AddInterface(idx uint64, name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.interfaces[idx] = name
	log.Debugf("NetworkMonitor: Added interface %d -> %s", idx, name)
}

func (a *NetworkMonitorAggregator) AddConnection(serial uint64, pid uint64, ifaceIdx uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()

	name, exists := a.interfaces[ifaceIdx]
	isValid := false
	if exists {
		isValid = isAllowedInterface(name)
	}

	a.connections[serial] = &ConnectionInfo{
		PID:            pid,
		InterfaceIndex: ifaceIdx,
		IsValid:        isValid,
	}
}

func (a *NetworkMonitorAggregator) UpdateConnection(serial uint64, rxBytes uint64, txBytes uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()

	conn, exists := a.connections[serial]
	if !exists || !conn.IsValid {
		return
	}

	// Calculate deltas since last update for this specific connection
	var deltaRx, deltaTx uint64
	if rxBytes > conn.LastRxBytes {
		deltaRx = rxBytes - conn.LastRxBytes
	}
	if txBytes > conn.LastTxBytes {
		deltaTx = txBytes - conn.LastTxBytes
	}

	conn.LastRxBytes = rxBytes
	conn.LastTxBytes = txBytes

	// Update PID totals
	if _, ok := a.pidBytesMap[conn.PID]; !ok {
		a.pidBytesMap[conn.PID] = &NetworkBytes{}
	}
	a.pidBytesMap[conn.PID].RxBytes += deltaRx
	a.pidBytesMap[conn.PID].TxBytes += deltaTx

	// Update Global totals
	a.globalBytes.RxBytes += deltaRx
	a.globalBytes.TxBytes += deltaTx
}

func (a *NetworkMonitorAggregator) GetNetworkBytes(pid uint64) (rx uint64, tx uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if pid == 0 {
		return a.globalBytes.RxBytes, a.globalBytes.TxBytes
	}

	if bytes, ok := a.pidBytesMap[pid]; ok {
		return bytes.RxBytes, bytes.TxBytes
	}
	return 0, 0
}

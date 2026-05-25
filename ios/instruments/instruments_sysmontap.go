package instruments

import (
	"fmt"
	"reflect"
	"strconv"

	"github.com/danielpaulus/go-ios/ios"
	dtx "github.com/danielpaulus/go-ios/ios/dtx_codec"
	log "github.com/sirupsen/logrus"
)

type sysmontapMsgDispatcher struct {
	messages chan dtx.Message
	conn     *dtx.Connection
}

func newSysmontapMsgDispatcher() *sysmontapMsgDispatcher {
	return &sysmontapMsgDispatcher{messages: make(chan dtx.Message, 1000)}
}

func (p *sysmontapMsgDispatcher) Dispatch(m dtx.Message) {
	if p.conn != nil {
		dtx.SendAckIfNeeded(p.conn, m)
	}
	p.messages <- m
}

const sysmontapName = "com.apple.instruments.server.services.sysmontap"

type sysmontapService struct {
	channel *dtx.Channel
	conn    *dtx.Connection

	deviceInfoService *DeviceInfoService
	msgDispatcher     *sysmontapMsgDispatcher
	procAttrs         []interface{}
	sysAttrs          []interface{}
}

// NewSysmontapService creates a new sysmontapService
// - samplingInterval is the rate how often to get samples, i.e Xcode's default is 10, which results in sampling output
// each 1 second, with 500 the samples are retrieved every 15 seconds. It doesn't make any correlation between
// the expected rate and the actual rate of samples delivery. We can only conclude, that the lower the rate in digits,
// the faster the samples are delivered
func NewSysmontapService(device ios.DeviceEntry, samplingInterval int) (*sysmontapService, error) {
	deviceInfoService, err := NewDeviceInfoService(device)
	if err != nil {
		return nil, err
	}

	msgDispatcher := newSysmontapMsgDispatcher()
	dtxConn, err := connectInstrumentsWithMsgDispatcher(device, msgDispatcher)
	if err != nil {
		return nil, err
	}
	msgDispatcher.conn = dtxConn

	processControlChannel := dtxConn.RequestChannelIdentifier(sysmontapName, msgDispatcher)

	sysAttrs, err := deviceInfoService.systemAttributes()
	if err != nil {
		return nil, err
	}

	procAttrs, err := deviceInfoService.processAttributes()
	if err != nil {
		return nil, err
	}

	config := map[string]interface{}{
		"ur":             samplingInterval,
		"bm":             0,
		"procAttrs":      procAttrs,
		"sysAttrs":       sysAttrs,
		"cpuUsage":       true,
		"physFootprint":  true,
		"processes":      true,
		"sampleInterval": 500000000,
	}
	_, err = processControlChannel.MethodCall("setConfig:", config)
	if err != nil {
		return nil, err
	}

	err = processControlChannel.MethodCallAsync("start")
	if err != nil {
		return nil, err
	}

	return &sysmontapService{processControlChannel, dtxConn, deviceInfoService, msgDispatcher, procAttrs, sysAttrs}, nil
}

// Close closes up the DTX connection, message dispatcher and dtx.Message channel
func (s *sysmontapService) Close() error {
	close(s.msgDispatcher.messages)

	s.deviceInfoService.Close()
	return s.conn.Close()
}

// GetProcAttrs returns the process attributes list
func (s *sysmontapService) GetProcAttrs() []interface{} {
	return s.procAttrs
}

// GetSysAttrs returns the system attributes list
func (s *sysmontapService) GetSysAttrs() []interface{} {
	return s.sysAttrs
}

// ReceiveMessage returns a chan of SysmontapMessage with CPU and memory info.
func (s *sysmontapService) ReceiveMessage() chan SysmontapMessage {
	messages := make(chan SysmontapMessage)
	go func() {
		defer close(messages)

		for msg := range s.msgDispatcher.messages {
			sysmontapMessage, err := mapToSysmonMessage(msg, s.procAttrs, s.sysAttrs)
			if err != nil {
				log.Debugf("expected `sysmontapMessage` from global channel, but received %v", msg)
				continue
			}

			messages <- sysmontapMessage
		}

		log.Infof("sysmontap message dispatcher channel closed")
	}()

	return messages
}

// Kept for backward compatibility
func (s *sysmontapService) ReceiveCPUUsage() chan SysmontapMessage {
	return s.ReceiveMessage()
}

// SysmontapMessage is a wrapper struct for incoming CPU/Mem samples
type SysmontapMessage struct {
	CPUCount       uint64
	EnabledCPUs    uint64
	EndMachAbsTime uint64
	Type           uint64
	SystemCPUUsage CPUUsage
	SystemMemory   map[string]interface{}
	Processes      map[uint64]ProcessMetrics
}

type CPUUsage struct {
	CPU_TotalLoad float64
}

type ProcessMetrics struct {
	CPUUsage      float64
	PhysFootprint uint64
}

// ToUint64 converts any numeric type to uint64. Returns false for
// non-numeric types or negative values.
func ToUint64(v interface{}) (uint64, bool) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if rv.Int() < 0 {
			return 0, false
		}
		return uint64(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return rv.Uint(), true
	case reflect.Float32, reflect.Float64:
		return uint64(rv.Float()), true
	case reflect.String:
		parsed, err := strconv.ParseUint(rv.String(), 10, 64)
		if err == nil {
			return parsed, true
		}
		return 0, false
	default:
		return 0, false
	}
}

// ToFloat64 converts any numeric type to float64. Returns false for
// non-numeric types.
func ToFloat64(v interface{}) (float64, bool) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		return rv.Float(), true
	case reflect.String:
		parsed, err := strconv.ParseFloat(rv.String(), 64)
		if err == nil {
			return parsed, true
		}
		return 0, false
	default:
		return 0, false
	}
}

// requireUint64 extracts a uint64 from a map field, accepting any numeric type.
func requireUint64(m map[string]interface{}, key string) (uint64, error) {
	v, ok := ToUint64(m[key])
	if !ok {
		return 0, fmt.Errorf("expected numeric %s, got %T: %+v", key, m[key], m[key])
	}
	return v, nil
}

// requireFloat64 extracts a float64 from a map field, accepting any numeric type.
func requireFloat64(m map[string]interface{}, key string) (float64, error) {
	v, ok := ToFloat64(m[key])
	if !ok {
		return 0, fmt.Errorf("expected numeric %s, got %T: %+v", key, m[key], m[key])
	}
	return v, nil
}

// requireMap extracts a nested map[string]interface{} from a map field.
// It also handles map[interface{}]interface{} generated by some decoders.
func requireMap(m map[string]interface{}, key string) (map[string]interface{}, error) {
	if val, ok := m[key]; ok {
		if mapVal, ok := val.(map[string]interface{}); ok {
			return mapVal, nil
		}
		// Handle map[interface{}]interface{} which is common in nskeyedarchiver
		if ifaceMap, ok := val.(map[interface{}]interface{}); ok {
			strMap := make(map[string]interface{})
			for k, v := range ifaceMap {
				strMap[fmt.Sprintf("%v", k)] = v
			}
			return strMap, nil
		}
		return nil, fmt.Errorf("expected map for %s, got %T: %+v", key, val, val)
	}
	return nil, fmt.Errorf("missing key %s", key)
}

// extractResultMap unwraps the DTX payload into the first result map.
// Payload structure: []interface{ []interface{ map[string]interface{}, ... }, ... }
func extractResultMap(payload []interface{}) (map[string]interface{}, error) {
	if len(payload) == 0 {
		return nil, fmt.Errorf("empty payload in sysmontap message")
	}

	resultArray, ok := payload[0].([]interface{})
	if !ok {
		return nil, fmt.Errorf("expected []interface{} as payload[0], got %T: %+v", payload[0], payload[0])
	}
	if len(resultArray) == 0 {
		return nil, fmt.Errorf("result array is empty in sysmontap payload: %+v", payload)
	}

	resultMap, ok := resultArray[0].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("expected map[string]interface{} for result, got %T: %+v", resultArray[0], resultArray[0])
	}
	return resultMap, nil
}

// mapToSysmonMessage parses a DTX sysmontap message into a SysmontapMessage.
func mapToSysmonMessage(msg dtx.Message, procAttrs []interface{}, sysAttrs []interface{}) (SysmontapMessage, error) {
	resultMap, err := extractResultMap(msg.Payload)
	if err != nil {
		return SysmontapMessage{}, err
	}

	cpuCount, err := requireUint64(resultMap, "CPUCount")
	if err != nil {
		return SysmontapMessage{}, err
	}
	enabledCPUs, err := requireUint64(resultMap, "EnabledCPUs")
	if err != nil {
		return SysmontapMessage{}, err
	}
	endMachAbsTime, err := requireUint64(resultMap, "EndMachAbsTime")
	if err != nil {
		return SysmontapMessage{}, err
	}
	typ, err := requireUint64(resultMap, "Type")
	if err != nil {
		return SysmontapMessage{}, err
	}

	sysCPUMap, err := requireMap(resultMap, "SystemCPUUsage")
	if err != nil {
		return SysmontapMessage{}, err
	}
	cpuTotalLoad, err := requireFloat64(sysCPUMap, "CPU_TotalLoad")
	if err != nil {
		return SysmontapMessage{}, err
	}

	var systemMemory map[string]interface{}
	if sysMemMap, err := requireMap(resultMap, "SystemMemory"); err == nil {
		systemMemory = sysMemMap
	} else if sysArr, ok := resultMap["System"].([]interface{}); ok && len(sysAttrs) > 0 {
		// iOS 17 fallback: System is an array mapped by sysAttrs
		systemMemory = make(map[string]interface{})
		pageSize := uint64(16384) // Default iOS page size
		systemMemory["pagesize"] = pageSize
		var active, wired, compressed uint64
		for i, attrNameIntf := range sysAttrs {
			attrName, _ := attrNameIntf.(string)
			if i < len(sysArr) {
				val, _ := ToUint64(sysArr[i])
				if attrName == "physMemSize" {
					systemMemory["mem_size"] = val * pageSize
				} else if attrName == "vmActiveCount" {
					active = val
				} else if attrName == "vmWireCount" {
					wired = val
				} else if attrName == "vmCompressorPageCount" {
					compressed = val
				}
			}
		}
		systemMemory["phys_footprint"] = (active + wired + compressed) * pageSize
	} else {
		log.Infof("DEBUG - Failed to extract SystemMemory: %v", err)
	}

	processes := make(map[uint64]ProcessMetrics)
	if procMap, err := requireMap(resultMap, "Processes"); err == nil {
		// Find indices
		cpuIdx, memIdx := -1, -1
		for i, attr := range procAttrs {
			if attrStr, ok := attr.(string); ok {
				if attrStr == "cpuUsage" {
					cpuIdx = i
				} else if attrStr == "physFootprint" {
					memIdx = i
				}
			}
		}

		for pidStr, procData := range procMap {
			pid, ok := ToUint64(pidStr)
			if !ok {
				continue
			}
			procArr, ok := procData.([]interface{})
			if !ok {
				continue
			}

			var metrics ProcessMetrics
			if cpuIdx >= 0 && cpuIdx < len(procArr) {
				if cpu, valid := ToFloat64(procArr[cpuIdx]); valid {
					metrics.CPUUsage = cpu
				}
			}
			if memIdx >= 0 && memIdx < len(procArr) {
				if mem, valid := ToUint64(procArr[memIdx]); valid {
					metrics.PhysFootprint = mem
				}
			}
			processes[pid] = metrics
		}
	}

	return SysmontapMessage{
		CPUCount:       cpuCount,
		EnabledCPUs:    enabledCPUs,
		EndMachAbsTime: endMachAbsTime,
		Type:           typ,
		SystemCPUUsage: CPUUsage{CPU_TotalLoad: cpuTotalLoad},
		SystemMemory:   systemMemory,
		Processes:      processes,
	}, nil
}

// Kept for backward compatibility
func mapToCPUUsage(msg dtx.Message) (SysmontapMessage, error) {
	return mapToSysmonMessage(msg, nil, nil)
}

package port

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

type Protocol string

const (
	ProtocolTCP  Protocol = "tcp"
	ProtocolUDP  Protocol = "udp"
	ProtocolSCTP Protocol = "sctp"
)

func (p Protocol) String() string {
	return string(p)
}

type Port struct {
	Number   int
	Protocol Protocol
}

func (p Port) String() string {
	return fmt.Sprintf("%d/%s", p.Number, p.Protocol)
}

type Mapping struct {
	TargetIP            string
	TargetPortNumber    int
	ContainerPortNumber int
	Protocol            Protocol

	// The raw mapping string as passed to the command line.
	raw string
}

func (m *Mapping) ContainerPort() Port {
	return Port{Number: m.ContainerPortNumber, Protocol: m.Protocol}
}

// TargetAddress returns the target address in format <host>:<port>.
func (m *Mapping) TargetAddress() string {
	return fmt.Sprintf("%s:%d", m.TargetIP, m.TargetPortNumber)
}

func CheckDuplicates(mm []Mapping) error {
	mapped := make(map[int][]*Mapping)
	for _, m := range mm {
		mapped[m.ContainerPortNumber] = append(mapped[m.ContainerPortNumber], &m)
	}
	// TODO: collect errors for multiple duplicates and return one error
	// comprising all ports with duplicate mappings
	for p := range mapped {
		if len(mapped[p]) > 1 {
			var rawMappings []string
			for _, m := range mapped[p] {
				rawMappings = append(rawMappings, m.raw)
			}
			return fmt.Errorf("container port %d mapped to multiple targets: %s", p, strings.Join(rawMappings, ", "))
		}
	}
	return nil
}

func ParsePort(rawPort string) (Port, error) {
	rawPortNum, rawProtocol := splitRawPort(rawPort)
	portNum, err := parsePortNumber(rawPortNum)
	if err != nil {
		return Port{}, fmt.Errorf("Invalid port number: \"%s\"", rawPortNum)
	}
	var protocol Protocol
	switch rawProtocol {
	case "udp":
		protocol = ProtocolUDP
	case "tcp":
		protocol = ProtocolTCP
	case "sctp":
		protocol = ProtocolSCTP
	default:
		// Note that rawProtocol comes as a return value from splitRawPort,
		// however its always retuning "tcp" or what the user specifed,
		// thus the error should make sense to the user.
		return Port{}, fmt.Errorf("Invalid port protocol: \"%s\"", rawProtocol)
	}

	return Port{Number: portNum, Protocol: protocol}, nil
}

func ParsePorts(rawPorts []string) ([]Port, error) {
	var pp []Port
	// TODO: collect errors for serveral ports and return one error
	// comprising all invalid ports
	for _, r := range rawPorts {
		p, err := ParsePort(r)
		if err != nil {
			return nil, err
		}
		pp = append(pp, p)
	}
	return pp, nil
}

func ParseMappings(rawMappings []string) ([]Mapping, error) {
	var mm []Mapping
	// TODO: collect errors for serveral mappings and return one error
	// comprising all invalid mappings
	for _, r := range rawMappings {
		m, err := ParseMapping(r)
		if err != nil {
			return nil, fmt.Errorf("argument \"%s\": %v", r, err)
		}
		mm = append(mm, m)
	}
	return mm, nil
}

func ParseMapping(rawMapping string) (Mapping, error) {
	rawTargetIP, rawTargetPortNum, rawContainerPort := splitRawMapping(rawMapping)

	// Validate and parse rawTargetIP.
	targetIP, _, err := net.SplitHostPort(rawTargetIP + ":") // Strip [] from IPV6 addresses
	if err != nil {
		return Mapping{}, fmt.Errorf("Invalid ip address %v: \"%s\"", rawTargetIP, err)
	}
	if targetIP != "" && net.ParseIP(targetIP) == nil {
		return Mapping{}, fmt.Errorf("Invalid ip address: \"%s\"", targetIP)
	}

	// Validate rawTargetPortNum.
	targetPortNum, err := parsePortNumber(rawTargetPortNum)
	if err != nil {
		return Mapping{}, fmt.Errorf("Invalid target port number: \"%s\"", rawTargetPortNum)
	}

	// Validate and parse containerPort.
	if rawContainerPort == "" {
		return Mapping{}, fmt.Errorf("No port specified: \"%s<empty>\"", rawMapping)
	}
	rawContainerPortNum, rawProtocol := splitRawPort(rawContainerPort)
	containerPortNum, err := parsePortNumber(rawContainerPortNum)
	if err != nil {
		return Mapping{}, fmt.Errorf("Invalid container port number: \"%s\"", rawContainerPortNum)
	}
	var protocol Protocol
	switch rawProtocol {
	case "udp":
		protocol = ProtocolUDP
	case "tcp":
		protocol = ProtocolTCP
	case "sctp":
		protocol = ProtocolSCTP
	default:
		// Note that rawProtocol comes as a return value from splitRawPort,
		// however its always retuning "tcp" or what the user specifed,
		// thus the error should make sense to the user.
		return Mapping{}, fmt.Errorf("Invalid container port protocol: \"%s\"", rawProtocol)
	}

	mapping := Mapping{
		TargetIP:            targetIP,
		TargetPortNumber:    targetPortNum,
		ContainerPortNumber: containerPortNum,
		Protocol:            protocol,
		raw:                 rawMapping,
	}
	return mapping, nil
}

// splitParts splits up a raw mapping string into its parts. Returns the target
// ip, target port number (without protocol) and the container port (if
// specified, including protocol).
//
// 	splitRawMapping("8080") -> "", "", "8080"
// 	splitRawMapping("8080/tcp") -> "", "", "8080/tcp"
// 	splitRawMapping("417:417/udp") -> "", "417", "417/tcp"
// 	splitRawMapping("127.0.0.1:80:8080/tcp") -> "127.0.0.1", "80", "8080/tcp"
//
// Nothing is validated by splitRawMapping.
func splitRawMapping(rawMapping string) (string, string, string) {
	parts := strings.Split(rawMapping, ":")
	n := len(parts)
	containerport := parts[n-1]

	switch n {
	case 1:
		return "", "", containerport
	case 2:
		return "", parts[0], containerport
	case 3:
		return parts[0], parts[1], containerport
	default:
		return strings.Join(parts[:n-2], ":"), parts[n-2], containerport
	}
}

// splitParts splits up a raw port string into its parts. Returns the port number
// and the protocol.
//
// 	splitRawPort("8080") -> "8080", "tcp"
// 	splitRawPort("8080/udp") -> "8080", "udp",
// 	splitRawPort("8080/") -> "8080", "tcp"
//
// 	splitRawPort("") -> "", ""
// 	splitRawPort("/udp") -> "", "udp"
// 	splitRawPort("8080/udp/8081") -> "8080", "udp"
//
// Nothing is validated by splitRawMapping.
func splitRawPort(rawPort string) (string, string) {
	parts := strings.Split(rawPort, "/")
	if len(rawPort) == 0 || len(parts) == 0 || len(parts[0]) == 0 {
		return "", ""
	}
	if len(parts) == 1 {
		return rawPort, "tcp"
	}
	if len(parts[1]) == 0 {
		return parts[0], "tcp"
	}
	return parts[0], parts[1]
}

// parsePortNumber parses n and returns it as an integer. Any error from
// strconv.ParseUint is returned.
func parsePortNumber(n string) (int, error) {
	port, err := strconv.ParseUint(n, 10, 16)
	if err != nil {
		return 0, err
	}
	return int(port), nil
}

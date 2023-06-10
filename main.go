package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var (
	flagCheckInterval    = flag.Duration("check-interval", 5*time.Second, "how often to check for upstream health")
	flagCheckIP          = flag.String("check-ip", "8.8.8.8", "IP address to check")
	flagPrimaryInterface = flag.String("primary", "", "primary interface name")
	flagPrimaryGateway   = flag.String("primary-gw", "", "primary gateway IP; autodetection attempted if not set")
	flagBackupInterface  = flag.String("backup", "", "backup interface name")
	flagBackupGateway    = flag.String("backup-gw", "", "backup gateway IP; autodetection attempted if not set")

	flagSystemdNetworkd = flag.Bool("systemd-networkd", false, "autodetect from systemd-networkd")
	flagDhcpcd          = flag.Bool("dhcpcd", false, "autodetect from dhcpcd")
)

func main() {
	flag.Parse()

	if *flagPrimaryInterface == "" {
		log.Fatalf("no primary interface provided")
	} else if *flagBackupInterface == "" {
		log.Fatalf("no backup interface provided")
	}

	primary, err := net.InterfaceByName(*flagPrimaryInterface)
	if err != nil {
		log.Fatalf("error getting primary interface %q: %v", *flagPrimaryInterface, err)
	}

	backup, err := net.InterfaceByName(*flagBackupInterface)
	if err != nil {
		log.Fatalf("error getting backup interface %q: %v", *flagBackupInterface, err)
	}

	//log.Printf("primary: %v", primary)
	//log.Printf("backup: %v", backup)

	primaryGw, err := parseOrGetGateway(*flagPrimaryGateway, primary)
	if err != nil {
		log.Fatalf("error detecting primary gateway: %v", err)
	}
	log.Printf("primary gateway: %q", primaryGw)

	backupGw, err := parseOrGetGateway(*flagBackupGateway, backup)
	if err != nil {
		log.Fatalf("error detecting backup gateway: %v", err)
	}
	log.Printf("backup gateway: %q", backupGw)
}

func parseOrGetGateway(val string, iface *net.Interface) (netip.Addr, error) {
	if val != "" {
		gw, err := netip.ParseAddr(val)
		if err == nil {
			return gw, nil
		}
	}

	gw, err := getGateway(iface)
	if err != nil {
		return netip.Addr{}, err
	}

	log.Printf("autodetected gateway for %s: %v", iface.Name, gw)
	return gw, nil
}

func getGateway(iface *net.Interface) (netip.Addr, error) {
	if *flagSystemdNetworkd {
		return getGatewaySystemdNetworkd(iface)
	} else if *flagDhcpcd {
		return getGatewayDhcpcd(iface)
	}

	return netip.Addr{}, errors.New("unimplemented")
}

func getGatewaySystemdNetworkd(iface *net.Interface) (netip.Addr, error) {
	leaseFile := filepath.Join("/run/systemd/netif/leases", strconv.Itoa(iface.Index))
	f, err := os.Open(leaseFile)
	if err != nil {
		return netip.Addr{}, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		if key == "ROUTER" {
			return netip.ParseAddr(value)
		}
	}

	return netip.Addr{}, fmt.Errorf("ROUTER not found in lease file")
}

func getGatewayDhcpcd(iface *net.Interface) (netip.Addr, error) {
	cmd := exec.Command("dhcpcd", "-U", iface.Name)
	out, err := cmd.Output()
	if err != nil {
		return netip.Addr{}, err
	}

	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if !ok {
			continue
		}
		if key == "routers" {
			return netip.ParseAddr(value)
		}
	}

	return netip.Addr{}, errors.New("routers not found in dhcpcd output")
}

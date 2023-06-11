package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
)

var (
	flagCheckInterval    = flag.Duration("check-interval", 5*time.Second, "how often to check for upstream health")
	flagCheckIP          = flag.String("check-ip", "8.8.8.8", "IP address to check") // TODO: IPv6 addr?
	flagPrimaryInterface = flag.String("primary", "", "primary interface name")
	flagPrimaryGateway   = flag.String("primary-gw", "", "primary gateway IP; autodetection attempted if not set")
	flagBackupInterface  = flag.String("backup", "", "backup interface name")
	flagBackupGateway    = flag.String("backup-gw", "", "backup gateway IP; autodetection attempted if not set")
	flagDryRun           = flag.Bool("dry-run", false, "if set, don't actually change route table")

	// TODO: set primary up/down if failed for long enough?

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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		cancel()
	}()

	ticker := time.NewTicker(*flagCheckInterval)
	defer ticker.Stop()

mainLoop:
	for {
		select {
		case <-ctx.Done():
			log.Printf("finished")
			break mainLoop
		case <-ticker.C:
			log.Printf("checking for internet status") // TODO: verbose only?
			if err := doCheckOnce(ctx, primary, primaryGw, backup, backupGw); err != nil {
				log.Printf("error checking: %v", err)
			}
		}
	}
}

func doCheckOnce(
	ctx context.Context,
	primary *net.Interface,
	primaryGw netip.Addr,
	backup *net.Interface,
	backupGw netip.Addr,
) error {
	currentGateway, err := getDefaultRouteInterface()
	if err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, "ping", "-I", primary.Name, "-c1", *flagCheckIP)
	cmd.Stdout = io.Discard // TODO: capture?
	cmd.Stderr = io.Discard

	err = cmd.Run()
	if err == nil {
		// Success; if we're using the backup interface, then switch to
		// the primary.
		if currentGateway == backup.Name {
			log.Printf("primary interface up; switching from backup -> primary")
			if !*flagDryRun {
				err = switchDefaultRoute(backup, backupGw, primary, primaryGw)
			}
		} else {
			// TODO: verbose only
			log.Printf("on primary interface; doing nothing")
		}
	} else {
		err = nil // maybe set below

		if currentGateway == primary.Name {
			log.Printf("primary interface down; switching from primary -> backup")
			if !*flagDryRun {
				err = switchDefaultRoute(primary, primaryGw, backup, backupGw)
			}
		} else {
			// TODO: verbose only
			log.Printf("on backup interface; doing nothing")
		}
	}

	// err is set above if any changes are made
	return err
}

var _, defaultDst, _ = net.ParseCIDR("0.0.0.0/0")

func switchDefaultRoute(oldDev *net.Interface, oldGw netip.Addr, newDev *net.Interface, newGw netip.Addr) error {
	err := netlink.RouteDel(&netlink.Route{
		Dst:       defaultDst,      // "default"
		LinkIndex: oldDev.Index,    // "dev backup"
		Gw:        oldGw.AsSlice(), // "via 1.2.3.4"
	})
	if err != nil {
		log.Printf("error removing old default route: %v", err)
	}
	return netlink.RouteAdd(&netlink.Route{
		Dst:       defaultDst,      // "default"
		LinkIndex: newDev.Index,    // "dev primary"
		Gw:        newGw.AsSlice(), // "via 5.6.7.8"
	})
}

func getDefaultRouteInterface() (string, error) {
	// TODO: parse from check IP
	dst := net.IPv4(8, 8, 8, 8)
	routes, err := netlink.RouteGet(dst)
	if err != nil {
		return "", err
	}
	if len(routes) == 0 {
		return "", fmt.Errorf("no routes to %v", dst)
	}

	iface, err := net.InterfaceByIndex(routes[0].LinkIndex)
	if err != nil {
		return "", fmt.Errorf("looking up link index %d: %w", routes[0].LinkIndex, err)
	}

	return iface.Name, nil
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

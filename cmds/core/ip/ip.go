// Copyright 2012-2017 the u-root Authors. All rights reserved
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	l "log"
	"net"
	"os"
	"strings"

	flag "github.com/spf13/pflag"

	"github.com/vishvananda/netlink"
)

var inet6 = flag.BoolP("6", "6", false, "use ipv6")

// The language implemented by the standard 'ip' is not super consistent
// and has lots of convenience shortcuts.
// The BNF the standard ip  shows you doesn't show many of these short cuts, and
// it is wrong in other ways.
// For this ip command:.
// The inputs is just the set of args.
// The input is very short -- it's not a program!
// Each token is just a string and we need not produce terminals with them -- they can
// just be the terminals and we can switch on them.
// The cursor is always our current token pointer. We do a simple recursive descent parser
// and accumulate information into a global set of variables. At any point we can see into the
// whole set of args and see where we are. We can indicate at each point what we're expecting so
// that in usage() or recover() we can tell the user exactly what we wanted, unlike the standard ip,
// which just dumps a whole (incorrect) BNF at you when you do anything wrong.
// To handle errors in too few arguments, we just do a recover block. That lets us blindly
// reference the arg[] array without having to check the length everywhere.

// RE: the use of globals. The reason is simple: we parse one command, do it, and quit.
// It doesn't make sense to write this otherwise.
var (
	// Cursor is out next token pointer.
	// The language of this command doesn't require much more.
	cursor    int
	arg       []string
	whatIWant []string
	log       = l.New(os.Stdout, "ip: ", 0)

	addrScopes = map[netlink.Scope]string{
		netlink.SCOPE_UNIVERSE: "global",
		netlink.SCOPE_HOST:     "host",
		netlink.SCOPE_SITE:     "site",
		netlink.SCOPE_LINK:     "link",
		netlink.SCOPE_NOWHERE:  "nowhere",
	}
)

// the pattern:
// at each level parse off arg[0]. If it matches, continue. If it does not, all error with how far you got, what arg you saw,
// and why it did not work out.

func usage() error {
	return fmt.Errorf("This was fine: '%v', and this was left, '%v', and this was not understood, '%v'; only options are '%v'",
		arg[0:cursor], arg[cursor:], arg[cursor], whatIWant)
}

func one(cmd string, cmds []string) string {
	var x, n int
	for i, v := range cmds {
		if strings.HasPrefix(v, cmd) {
			n++
			x = i
		}
	}
	if n == 1 {
		return cmds[x]
	}
	return ""
}

// in the ip command, turns out 'dev' is a noise word.
// The BNF it shows is not right in that case.
// Always make 'dev' optional.
func dev() (netlink.Link, error) {
	cursor++
	whatIWant = []string{"dev", "device name"}
	if arg[cursor] == "dev" {
		cursor++
	}
	whatIWant = []string{"device name"}
	return netlink.LinkByName(arg[cursor])
}

func addrip() error {
	var err error
	var addr *netlink.Addr
	if len(arg) == 1 {
		return showLinks(os.Stdout, true)
	}
	cursor++
	whatIWant = []string{"add", "del"}
	cmd := arg[cursor]

	c := one(cmd, whatIWant)
	switch c {
	case "add", "del":
		cursor++
		whatIWant = []string{"CIDR format address"}
		addr, err = netlink.ParseAddr(arg[cursor])
		if err != nil {
			return err
		}
	default:
		return usage()
	}
	iface, err := dev()
	if err != nil {
		return err
	}
	switch c {
	case "add":
		if err := netlink.AddrAdd(iface, addr); err != nil {
			return fmt.Errorf("Adding %v to %v failed: %v", arg[1], arg[2], err)
		}
	case "del":
		if err := netlink.AddrDel(iface, addr); err != nil {
			return fmt.Errorf("Deleting %v from %v failed: %v", arg[1], arg[2], err)
		}
	default:
		return fmt.Errorf("devip: arg[0] changed: can't happen")
	}
	return nil
}

func neigh() error {
	if len(arg) != 1 {
		return errors.New("neigh subcommands not supported yet")
	}
	return showNeighbours(os.Stdout, true)
}

func linkshow() error {
	cursor++
	whatIWant = []string{"<nothing>", "<device name>"}
	if len(arg[cursor:]) == 0 {
		return showLinks(os.Stdout, false)
	}
	return nil
}

func setHardwareAddress(iface netlink.Link) error {
	cursor++
	hwAddr, err := net.ParseMAC(arg[cursor])
	if err != nil {
		return fmt.Errorf("%v cant parse mac addr %v: %v", iface.Attrs().Name, hwAddr, err)
	}
	err = netlink.LinkSetHardwareAddr(iface, hwAddr)
	if err != nil {
		return fmt.Errorf("%v cant set mac addr %v: %v", iface.Attrs().Name, hwAddr, err)
	}
	return nil
}

func linkset() error {
	iface, err := dev()
	if err != nil {
		return err
	}

	cursor++
	whatIWant = []string{"address", "up", "down"}
	switch one(arg[cursor], whatIWant) {
	case "address":
		return setHardwareAddress(iface)
	case "up":
		if err := netlink.LinkSetUp(iface); err != nil {
			return fmt.Errorf("%v can't make it up: %v", iface.Attrs().Name, err)
		}
	case "down":
		if err := netlink.LinkSetDown(iface); err != nil {
			return fmt.Errorf("%v can't make it down: %v", iface.Attrs().Name, err)
		}
	default:
		return usage()
	}
	return nil
}

func link() error {
	if len(arg) == 1 {
		return linkshow()
	}

	cursor++
	whatIWant = []string{"show", "set"}
	cmd := arg[cursor]

	switch one(cmd, whatIWant) {
	case "show":
		return linkshow()
	case "set":
		return linkset()
	}
	return usage()
}

func hexToBytes(xs string) ([]byte, error) {
	x, err := hex.DecodeString(xs)
	if err != nil {
		return nil, err
	}
	if len(x) != net.IPv4len && len(x) != net.IPv6len {
		return nil, fmt.Errorf("invalid IP address length: %d", len(x))
	}
	// entries are in network byte order, needs to be swapped
	for i := 0; i < len(x)/2; i++ {
		x[i], x[len(x)-i-1] = x[len(x)-i-1], x[i]
	}
	return x, nil
}

func hexToIP(xs string) (net.IP, error) {
	x, err := hexToBytes(xs)
	return net.IP(x), err
}

func printRoutev6(sc *bufio.Scanner) error {
	for sc.Scan() {
		fmt.Println(sc.Text())
	}
	return sc.Err()
}

// printRoutev4 interprets the content of /proc/net/route and prints as much as
// possible according to the output of `ip route show` from iproute2. However
// /proc/net/route does not contain all the necessary information, which should
// be retrieved via rtnetlink instead. But at least now we can print some
// interpreted route information.
func printRoutev4(sc *bufio.Scanner) error {
	expectedHeader := []string{
		// fields from /proc/net/route
		"Iface",
		"Destination",
		"Gateway",
		"Flags",
		"RefCnt",
		"Use",
		"Metric",
		"Mask",
		"MTU",
		"Window",
		"IRTT",
	}
	var lineno uint64
	for sc.Scan() {
		lineno++
		fields := strings.Fields(sc.Text())
		if len(fields) != len(expectedHeader) {
			return fmt.Errorf("cannot parse IPv4 route entry: expected %d fields, got %d", len(expectedHeader), len(fields))
		}
		if lineno == 1 {
			// parse as header
			for i := 0; i < len(expectedHeader); i++ {
				if fields[i] != expectedHeader[i] {
					return fmt.Errorf("Invalid '%s' field at position %d: want %s", fields[i], i, expectedHeader[i])
				}
			}
		} else {
			// parse as entry
			var out string
			// parse destination
			dest, err := hexToIP(fields[1])
			if err != nil {
				return fmt.Errorf("invalid hex-formatted destination IP %s: %v", fields[1], err)
			}
			if dest.Equal(net.IPv4zero) {
				out += "default"
			} else {
				out += dest.String()
				// add netmask
				mask, err := hexToBytes(fields[7])
				if err != nil {
					return fmt.Errorf("invalid hex-formatted netmask %s: %v", fields[7], err)
				}
				ones, _ := net.IPMask(mask).Size()
				out += fmt.Sprintf("/%d", ones)
			}
			// print gateway, if any
			gw, err := hexToIP(fields[2])
			if err != nil {
				return fmt.Errorf("invalid hex-formatted gateway IP %s: %v", fields[2], err)
			}
			if !gw.Equal(net.IPv4zero) {
				out += " via " + gw.String()
			}
			// print interface
			out += " dev " + fields[0]
			// print metric
			// TODO check that metric is a valid positive integer string
			out += " metric " + fields[6]
			// TODO print proto, scope, src, status. This information is not
			// present in /proc/net/route and needs to be retrieved via
			// rtnetlink.
			fmt.Println(out)
		}
	}
	return sc.Err()
}

func routeshow() error {
	path := "/proc/net/route"
	if *inet6 {
		path = "/proc/net/ipv6_route"
	}
	fd, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open %s: %v", path, err)
	}
	defer func() {
		if err := fd.Close(); err != nil {
			log.Printf("Warning: failed to close %s: %v", path, err)
		}
	}()
	sc := bufio.NewScanner(fd)
	if *inet6 {
		err = printRoutev6(sc)
	} else {
		err = printRoutev4(sc)
	}
	if err != nil {
		return fmt.Errorf("failed to parse %s: %v", path, err)
	}
	return nil
}

func nodespec() string {
	cursor++
	whatIWant = []string{"default", "CIDR"}
	return arg[cursor]
}

func nexthop() (string, *netlink.Addr, error) {
	cursor++
	whatIWant = []string{"via"}
	if arg[cursor] != "via" {
		return "", nil, usage()
	}
	nh := arg[cursor]
	cursor++
	whatIWant = []string{"Gateway CIDR"}
	addr, err := netlink.ParseAddr(arg[cursor])
	if err != nil {
		return "", nil, fmt.Errorf("Gateway CIDR: %v", err)
	}
	return nh, addr, nil
}

func routeadddefault() error {
	nh, nhval, err := nexthop()
	if err != nil {
		return err
	}
	// TODO: NHFLAGS.
	l, err := dev()
	if err != nil {
		return err
	}
	switch nh {
	case "via":
		log.Printf("Add default route %v via %v", nhval, l.Attrs().Name)
		r := &netlink.Route{LinkIndex: l.Attrs().Index, Gw: nhval.IPNet.IP}
		if err := netlink.RouteAdd(r); err != nil {
			return fmt.Errorf("error adding default route to %v: %v", l.Attrs().Name, err)
		}
		return nil
	}
	return usage()
}

func routeadd() error {
	ns := nodespec()
	switch ns {
	case "default":
		return routeadddefault()
	default:
		addr, err := netlink.ParseAddr(arg[cursor])
		if err != nil {
			return usage()
		}
		d, err := dev()
		if err != nil {
			return usage()
		}
		r := &netlink.Route{LinkIndex: d.Attrs().Index, Dst: addr.IPNet}
		if err := netlink.RouteAdd(r); err != nil {
			return fmt.Errorf("error adding route %s -> %s: %v", addr, d.Attrs().Name, err)
		}
		return nil
	}
}

func route() error {
	cursor++
	if len(arg[cursor:]) == 0 {
		return routeshow()
	}

	whatIWant = []string{"show", "add"}
	switch one(arg[cursor], whatIWant) {
	case "show":
		return routeshow()
	case "add":
		return routeadd()
	}
	return usage()
}

func main() {
	// When this is embedded in busybox we need to reinit some things.
	whatIWant = []string{"addr", "route", "link", "neigh"}
	cursor = 0
	flag.Parse()
	arg = flag.Args()

	defer func() {
		switch err := recover().(type) {
		case nil:
		case error:
			if strings.Contains(err.Error(), "index out of range") {
				log.Fatalf("Args: %v, I got to arg %v, I wanted %v after that", arg, cursor, whatIWant)
			} else if strings.Contains(err.Error(), "slice bounds out of range") {
				log.Fatalf("Args: %v, I got to arg %v, I wanted %v after that", arg, cursor, whatIWant)
			}
			log.Fatalf("Bummer: %v", err)
		default:
			log.Fatalf("unexpected panic value: %T(%v)", err, err)
		}
	}()

	// The ip command doesn't actually follow the BNF it prints on error.
	// There are lots of handy shortcuts that people will expect.
	var err error
	switch one(arg[cursor], whatIWant) {
	case "addr":
		err = addrip()
	case "link":
		err = link()
	case "route":
		err = route()
	case "neigh":
		err = neigh()
	default:
		usage()
	}
	if err != nil {
		log.Fatal(err)
	}

}

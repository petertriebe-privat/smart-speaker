#!/bin/sh
# udhcpc passes interface, ip, subnet, router and dns in the environment.
# shellcheck disable=SC2154
case "$1" in
deconfig)
    ip addr flush dev "$interface"
    ;;
bound|renew)
    ifconfig "$interface" "$ip" netmask "$subnet"
    if [ -n "$router" ]; then
        ip route replace default via "${router%% *}" dev "$interface"
    fi
    if [ -n "$dns" ]; then
        : > /etc/resolv.conf
        for server in $dns; do printf 'nameserver %s\n' "$server" >> /etc/resolv.conf; done
        # A public resolver after the router's (the router's DNS failed for an hour on 30.10.2026)
        printf 'nameserver 1.1.1.1\noptions timeout:2 attempts:2\n' >> /etc/resolv.conf
    fi
    ;;
esac

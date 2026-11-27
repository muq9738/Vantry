//! LAN candidates for the native TLS gateway and Alma's encrypted listener.
use if_addrs::{IfAddr, IfOperStatus, Interface};
use std::{
    io,
    net::{IpAddr, SocketAddr},
};

fn interfaces() -> io::Result<Vec<Interface>> {
    let mut nics = if_addrs::get_if_addrs()?;
    nics.retain(|nic| {
        let ip = nic.ip();
        matches!(nic.oper_status, IfOperStatus::Up | IfOperStatus::Unknown)
            && !ip.is_loopback()
            && !ip.is_unspecified()
            && !ip.is_multicast()
            && match ip {
                IpAddr::V4(v4) => {
                    !v4.is_link_local()
                        && !v4.is_broadcast()
                        && !matches!(v4.octets(), [198, 18 | 19, _, _])
                }
                IpAddr::V6(v6) => !v6.is_unicast_link_local(),
            }
    });
    nics.sort_by_key(|nic| {
        let name = nic.name.to_ascii_lowercase();
        let virtual_link = nic.is_p2p
            || [
                "utun", "tun", "tap", "bridge", "docker", "veth", "vmnet", "awdl", "llw",
            ]
            .iter()
            .any(|p| name.starts_with(p));
        let physical = ["en", "eth", "wl", "wi-fi"]
            .iter()
            .any(|p| name.starts_with(p));
        (
            virtual_link,
            !physical,
            nic.index.unwrap_or(u32::MAX),
            nic.ip(),
        )
    });
    Ok(nics)
}

pub fn listener_addresses(bound: SocketAddr) -> io::Result<Vec<SocketAddr>> {
    if !bound.ip().is_unspecified() {
        return Ok(vec![bound]);
    }
    let mut addresses = Vec::new();
    for nic in interfaces()? {
        let addr = SocketAddr::new(nic.ip(), bound.port());
        if addr.is_ipv4() == bound.is_ipv4() && !addresses.contains(&addr) {
            addresses.push(addr);
        }
    }
    Ok(addresses)
}

fn same_subnet(ip: IpAddr, nic: &Interface) -> bool {
    match (ip, &nic.addr) {
        (IpAddr::V4(ip), IfAddr::V4(local)) => {
            local.prefixlen > 0
                && (u32::from(ip) ^ u32::from(local.ip)) & u32::from(local.netmask) == 0
        }
        (IpAddr::V6(ip), IfAddr::V6(local)) => {
            local.prefixlen > 0
                && (u128::from(ip) ^ u128::from(local.ip)) & u128::from(local.netmask) == 0
        }
        _ => false,
    }
}

/// Older pairings can list virtual interfaces before the Wi-Fi or Ethernet address.
pub(crate) fn direct_candidates(saved: &[String]) -> Vec<&String> {
    let mut addresses: Vec<_> = saved.iter().collect();
    if addresses.len() > 1
        && let Ok(nics) = interfaces()
    {
        addresses.sort_by_key(|address| {
            let Ok(addr) = address.parse::<SocketAddr>() else {
                return usize::MAX;
            };
            if addr.ip().is_loopback() {
                return 0;
            }
            nics.iter()
                .position(|nic| same_subnet(addr.ip(), nic))
                .unwrap_or(usize::MAX)
        });
    }
    addresses
}

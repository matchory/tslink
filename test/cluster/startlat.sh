#!/bin/sh
# startlat.sh <since>: on a node, seconds from each endpoint's Join to its Tailscale IP since <since>
# shellcheck disable=SC2016 # awk program
grep -a -E 'msg="Join: network=|got Tailscale IP' /var/lib/docker-plugins/tailscale/plugin.log |
	awk -v since="time=$1" '$1 >= since' | awk '
  { split($1,a,"T"); split(a[2],b,"Z"); split(b[1],c,":"); t=c[1]*3600+c[2]*60+c[3] }
  /Join: network=/ { match($0,/endpoint=[0-9a-f]+/); id=substr($0,RSTART+9,12); j[id]=t }
  /got Tailscale IP/ { match($0,/Endpoint [0-9a-f]+/); id=substr($0,RSTART+9,12); if (id in j) { d=t-j[id]; n++; s+=d; if(d>m)m=d; if(!mi||d<mi)mi=d } }
  END { if(n) printf "n=%d min=%.1fs avg=%.1fs max=%.1fs\n", n, mi, s/n, m; else print "n=0" }'

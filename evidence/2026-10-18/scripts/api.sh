#!/bin/sh
# usage: api.sh <oke|kw> METHOD PATH [curl args...]
# Reads the automation token from ~/.kuvryn-ant/<ctx>-automation-token.txt with jq (never printed).
set -eu
inst=$1 m=$2 p=$3
shift 3
case $inst in
oke) url=https://ant.azrty.com ctx=azrty-oke ;;
kw) url=https://ant.kw.watteel.lab ctx=kw ;;
*)
	echo "bad instance" >&2
	exit 2
	;;
esac
tok=$(jq -r .secret "$HOME/.kuvryn-ant/$ctx-automation-token.txt")
exec curl -sk -X "$m" -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' "$@" "$url$p"

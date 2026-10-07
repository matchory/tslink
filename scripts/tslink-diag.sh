#!/bin/bash
# tslink-diag.sh - Run diagnostics on tslink Docker network plugin
#
# Usage:
#   ./tslink-diag.sh           # Run diagnostics
#   ./tslink-diag.sh --json    # Output as JSON
#   ./tslink-diag.sh --watch   # Watch mode (refresh every 5s)

set -e

DATA_DIR="/var/lib/docker-plugins/tailscale"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

print_header() {
    echo -e "${GREEN}=== tslink Diagnostics ===${NC}"
    echo "Time: $(date)"
    echo ""
}

# Prints "<name> <enabled>" for each installed tslink plugin. Plugins are
# recognised by their entrypoint, since they can be installed under any name.
tslink_plugins() {
    for id in $(docker plugin ls -q 2>/dev/null); do
        docker plugin inspect "$id" \
            --format '{{.Name}} {{.Enabled}} {{join .Config.Entrypoint " "}}' 2>/dev/null
    done | awk '$3 == "/tslink" && NF == 3 { print $1, $2 }'
}

check_plugin_status() {
    echo -e "${YELLOW}Plugin Status:${NC}"
    PLUGINS=$(tslink_plugins)
    if [ -z "$PLUGINS" ]; then
        echo -e "  Plugin: ${RED}not installed${NC}"
        return 1
    fi
    echo "$PLUGINS" | while read -r name enabled; do
        if [ "$enabled" = "true" ]; then
            echo -e "  $name: ${GREEN}enabled${NC}"
        else
            echo -e "  $name: ${RED}disabled${NC}"
        fi
    done
    echo ""
}

check_networks() {
    echo -e "${YELLOW}Networks using tslink:${NC}"
    NETWORKS=$(tslink_plugins | while read -r name _; do
        docker network ls --filter driver="$name" --format '{{.Name}}' 2>/dev/null
    done)
    if [ -z "$NETWORKS" ]; then
        echo "  (none)"
    else
        echo "$NETWORKS" | while read -r net; do
            CONTAINERS=$(docker network inspect "$net" --format '{{range .Containers}}{{.Name}} {{end}}' 2>/dev/null || true)
            echo "  $net: ${CONTAINERS:-no containers}"
        done
    fi
    echo ""
}

run_diag() {
    echo -e "${YELLOW}Endpoint Status:${NC}"
    # Run the diag command inside the plugin's data directory
    docker run --rm \
        -v "$DATA_DIR:/data:ro" \
        alpine sh -c '
            echo ""
            found=no
            # State: by-hostname/<hostname>, or by-stack/<stack>/<hostname> for stack tasks
            for dir in /data/by-hostname/*/ /data/by-stack/*/*/; do
                [ -d "$dir" ] || continue
                found=yes
                socket_exists="no"
                state_exists="no"
                # tailscaled.sock links to the daemon socket under sock/
                [ -S "${dir}tailscaled.sock" ] && socket_exists="yes"
                [ -f "${dir}tailscaled.state" ] && state_exists="yes"

                echo "  State dir: ${dir#/data/}"
                echo "    Socket: $socket_exists"
                echo "    State:  $state_exists"

                if [ -f "${dir}debug.log" ]; then
                    echo "    Debug log:"
                    sed "s/^/      /" "${dir}debug.log"
                fi
                echo ""
            done

            # Running daemons: sock/<endpoint-id[:12]>.sock
            for sock in /data/sock/*.sock; do
                [ -S "$sock" ] || continue
                found=yes
                echo "  Running tailscaled: $(basename "$sock" .sock)"
            done

            [ "$found" = "yes" ] || echo "  No endpoints found"
        '
}

show_recent_logs() {
    echo -e "${YELLOW}Recent Plugin Logs:${NC}"
    # Try to get Docker daemon logs (works on Linux)
    if command -v journalctl &> /dev/null; then
        journalctl -u docker --since "5 minutes ago" 2>/dev/null | \
            grep -E "tailscale|tslink|Endpoint|Join|Leave" | \
            tail -20 | sed 's/^/  /' || echo "  (could not read logs)"
    else
        echo "  (journalctl not available - check Docker Desktop logs)"
    fi
    echo ""
}

# Main
case "${1:-}" in
    --watch)
        while true; do
            clear
            print_header
            check_plugin_status
            check_networks
            run_diag
            echo "Refreshing in 5s... (Ctrl+C to exit)"
            sleep 5
        done
        ;;
    --json)
        docker run --rm \
            -v "$DATA_DIR:/data:ro" \
            alpine sh -c '
                echo "{"
                echo "  \"timestamp\": \"$(date -Iseconds)\","
                echo "  \"state_dirs\": ["
                first=true
                for dir in /data/by-hostname/*/ /data/by-stack/*/*/; do
                    [ -d "$dir" ] || continue

                    [ "$first" = "false" ] && echo ","
                    first=false

                    socket_exists="false"
                    state_exists="false"
                    [ -S "${dir}tailscaled.sock" ] && socket_exists="true"
                    [ -f "${dir}tailscaled.state" ] && state_exists="true"

                    rel="${dir#/data/}"
                    echo "    {"
                    echo "      \"path\": \"${rel%/}\","
                    echo "      \"socket_exists\": $socket_exists,"
                    echo "      \"state_exists\": $state_exists"
                    echo -n "    }"
                done
                echo ""
                echo "  ],"
                echo "  \"running\": ["
                first=true
                for sock in /data/sock/*.sock; do
                    [ -S "$sock" ] || continue
                    [ "$first" = "false" ] && echo ","
                    first=false
                    echo -n "    \"$(basename "$sock" .sock)\""
                done
                echo ""
                echo "  ]"
                echo "}"
            '
        ;;
    *)
        print_header
        check_plugin_status || exit 1
        check_networks
        run_diag
        ;;
esac

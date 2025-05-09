#!/usr/bin/env bash

devs=()
ports=()
idxes=()
rdmaIdx=""
rdmaRate=""
rdmaErr=""

function scan_devices() {
    allocDevs=()
    echo "[scan_devices] Start scanning RDMA devices..."
    for d in $(ls /sys/class/infiniband/) ; do
        echo "[scan_devices] Found Infiniband device: $d"
        allocDevs+=($d)
    done

    for d in "${allocDevs[@]}" ; do
        for p in $(ls /sys/class/infiniband/$d/ports/) ; do
            for g in $(ls /sys/class/infiniband/$d/ports/$p/gids/) ; do
                gid=$(cat /sys/class/infiniband/$d/ports/$p/gids/$g)
                echo "[scan_devices] Checking $d port $p gid $g = $gid"

                if [ $(echo $gid | cut -d ":" -f -1) != "0000" ] ; then
                    echo "[scan_devices] Skipped: Not a RoCE GID (not starting with 0000)"
                    continue
                fi

                if [ "$gid" = "0000:0000:0000:0000:0000:0000:0000:0000" ]; then
                    echo "[scan_devices] Skipped: GID is all-zero"
                    continue
                fi

                RoCEVer=$(cat /sys/class/infiniband/$d/ports/$p/gid_attrs/types/$g 2>/dev/null | grep -o "[Vv].*")
                echo "[scan_devices] RoCE version for $d:$p:$g = $RoCEVer"

                if [ "${RoCEVer,,}" != "v2" ]; then
                    echo "[scan_devices] Skipped: Not RoCEv2"
                    continue
                fi

                ethDev=$(cat /sys/class/infiniband/$d/ports/$p/gid_attrs/ndevs/$g 2>/dev/null)
                if [[ -z "$ethDev" ]]; then
                    echo "[scan_devices] Skipped: No netdev found"
                    continue
                fi

                carrierFile="/sys/class/net/$ethDev/carrier"
                if [[ ! -f $carrierFile ]] || [[ "$(cat $carrierFile 2>/dev/null)" != "1" ]]; then
                    echo "[scan_devices] Skipped: Interface $ethDev not up (carrier!=1)"
                    continue
                fi

                echo "[scan_devices] Added valid RDMA device: $d port $p gid $g"
                devs+=($d)
                ports+=($p)
                idxes+=($g)
            done
        done
    done

    if [[ ${#devs[@]} -eq 0 ]]; then
        echo "[scan_devices] ERROR: No valid RDMA device found"
        return 1
    fi
}

function export_nccl_env() {
    scan_devices
    if [[ ${#devs[@]} -eq 0 ]]; then
        echo "[export_nccl_env] ERROR: No RDMA devices found after scanning"
        return 1
    fi

    echo "[export_nccl_env] Start selecting RDMA device with highest rate"
    for ((i=0; i<${#devs[@]}; i++)); do
        rateFile="/sys/class/infiniband/${devs[i]}/ports/${ports[i]}/rate"
        rate=$(cat "$rateFile" 2>/dev/null | cut -f 1 -d " ")
        echo "[export_nccl_env] Device=${devs[i]}, Port=${ports[i]}, GID=${idxes[i]}, Rate=${rate} Gbps"

        if [[ -z "$rdmaRate" ]] || [[ "$rate" -gt "$rdmaRate" ]]; then
            rdmaDevs=("${devs[i]}")
            rdmaPorts=("${ports[i]}")
            rdmaIdx=${idxes[i]}
            rdmaErr=""
            rdmaRate=$rate
        elif [[ "$rate" == "$rdmaRate" ]]; then
            rdmaDevs+=("${devs[i]}")
            rdmaPorts+=("${ports[i]}")
            if [[ "$rdmaIdx" != "${idxes[i]}" ]]; then
                rdmaErr="ERROR: GID index mismatch across equal-rate devices (got $rdmaIdx vs ${idxes[i]})"
            fi
        fi
    done

    if [[ -n "$rdmaErr" ]]; then
        echo "[export_nccl_env] $rdmaErr"
        return 1
    fi

    hca=()
    for ((i=0; i<${#rdmaDevs[@]}; i++)); do
        hca+=("${rdmaDevs[i]}:${rdmaPorts[i]}")
    done

    IFS=","
    export NCCL_IB_HCA="${hca[*]}"
    unset IFS
    export NCCL_IB_GID_INDEX="$rdmaIdx"

    echo "[export_nccl_env] NCCL_IB_HCA=${NCCL_IB_HCA}"
    echo "[export_nccl_env] NCCL_IB_GID_INDEX=${NCCL_IB_GID_INDEX}"
    echo "[export_nccl_env] NCCL_IB device selection complete"
}

export_nccl_env

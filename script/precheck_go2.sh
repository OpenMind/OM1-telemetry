set -u
PASS=0
FAIL=0
WARN=0


ok()    { echo "  ✅ $1"; PASS=$((PASS+1)); }
fail()  { echo "  ❌ $1"; FAIL=$((FAIL+1)); }
warn()  { echo "  ⚠️  $1"; WARN=$((WARN+1)); }
hdr()   { echo ""; echo "═══ $1 ═══"; }

check_topic_exists() {
    local topic="$1"
    if ros2 topic list 2>/dev/null | grep -q "^${topic}\$"; then
        return 0
    fi
    return 1
}

check_topic_hz() {
    local topic="$1"
    local result
    result=$(timeout 4 ros2 topic hz "$topic" 2>&1 | grep -oP 'average rate: \K[\d.]+' | head -1)
    if [ -z "$result" ]; then
        echo "0"
        return 1
    fi
    echo "$result"
    return 0
}

check_topic_decodable() {
    local topic="$1"
    if timeout 3 ros2 topic echo --once "$topic" 2>&1 | grep -q "invalid"; then
        return 1
    fi
    return 0
}

echo "╔═══════════════════════════════════════════════════════════════╗"
echo "║         Go2 Recording Pre-Trip Check                          ║"
echo "║         Profile: 2D lidar /scan + depth + lowstate + RTSP     ║"
echo "╚═══════════════════════════════════════════════════════════════╝"


hdr "Required ROS topics (for the recorders that ARE enabled on Go2)"

# lidar (2D scan)  — ENABLED
if check_topic_exists "/scan"; then
    rate=$(check_topic_hz "/scan" "5")
    if [ "$rate" != "0" ]; then
        ok "/scan @ ${rate} Hz (2D laser scan, recorded by lidar recorder)"
    else
        fail "/scan exists but no data flowing"
    fi
else
    fail "/scan not found — lidar recorder will get nothing (this is Go2's primary recorded LiDAR source)"
fi

# depth — ENABLED
DEPTH="/camera/realsense2_camera_node/depth/image_rect_raw"
if check_topic_exists "$DEPTH"; then
    rate=$(check_topic_hz "$DEPTH" "10")
    if [ "$rate" != "0" ]; then
        ok "$DEPTH @ ${rate} Hz"
        rate_int=${rate%.*}
        if [ "$rate_int" -lt 10 ]; then
            warn "  depth rate is low (${rate} Hz, expected ~30); check RealSense fps config"
        fi
    else
        fail "$DEPTH exists but no data"
    fi
else
    fail "$DEPTH not found"
fi

# odom — ENABLED
if check_topic_exists "/odom"; then
    rate=$(check_topic_hz "/odom" "5")
    if [ "$rate" != "0" ]; then
        ok "/odom @ ${rate} Hz"
    else
        warn "/odom exists but no data flowing"
    fi
else
    fail "/odom not found"
fi

# lowstate — ENABLED
if check_topic_exists "/lowstate"; then
    rate=$(check_topic_hz "/lowstate" "100")
    if [ "$rate" != "0" ]; then
        ok "/lowstate @ ${rate} Hz"
        if check_topic_decodable "/lowstate"; then
            ok "  /lowstate message decodable (unitree_go installed)"
        else
            fail "  /lowstate message type INVALID — install unitree_go ROS package"
        fi
    else
        fail "/lowstate exists but no data"
    fi
else
    fail "/lowstate not found"
fi


hdr "RTSP streams (for video + audio recorders)"

check_rtsp() {
    local url="$1"
    local label="$2"
    local info
    # 6-second timeout: some streams (non-standard H.264 profiles like
    # yuv444p / High 4:4:4 Predictive) need extra time for ffprobe to
    # negotiate and read first frames.  Empirically 3 s was sometimes
    # not enough on Go2's down_camera.
    info=$(timeout 6 ffprobe -v error -show_streams "$url" 2>&1 | grep -oP 'codec_name=\K\w+' | head -1)
    if [ -n "$info" ]; then
        ok "$label ($url) → codec: $info"
    else
        fail "$label ($url) → not reachable (try manually: timeout 10 ffprobe -v error -show_streams $url)"
    fi
}

check_rtsp "rtsp://localhost:8554/top_camera_raw"   "video top_camera"
check_rtsp "rtsp://localhost:8554/front_camera"     "video front_camera"
check_rtsp "rtsp://localhost:8554/down_camera"      "video down_camera"
check_rtsp "rtsp://localhost:8554/audio"            "audio"


hdr "Calibration / latched topics (dump once per session for sensor fusion)"

for topic in /tf_static \
             /camera/realsense2_camera_node/color/camera_info \
             /camera/realsense2_camera_node/depth/camera_info \
             /camera/realsense2_camera_node/extrinsics/depth_to_color; do
    if check_topic_exists "$topic"; then
        if timeout 3 ros2 topic echo --once "$topic" >/dev/null 2>&1; then
            ok "$topic decodable"
        else
            warn "$topic exists but cannot echo"
        fi
    else
        warn "$topic not found"
    fi
done


# Go2 profile expectations
GO2_EXPECTED_ENABLE_LIDAR="true"
GO2_EXPECTED_ENABLE_POINTCLOUD="false"

# ENABLE_LIDAR should be true (or unset, which defaults true)
if [ "${ENABLE_LIDAR:-true}" = "true" ]; then
    ok "ENABLE_LIDAR=true (lidar recorder will run, recording /scan)"
else
    fail "ENABLE_LIDAR=${ENABLE_LIDAR} but Go2 profile expects true"
fi

# ENABLE_POINTCLOUD should be false for Go2 (the 3D Livox topic doesn't exist on Go2)
if [ "${ENABLE_POINTCLOUD:-true}" = "false" ]; then
    ok "ENABLE_POINTCLOUD=false (pointcloud recorder disabled — correct for Go2)"
elif [ -z "${ENABLE_POINTCLOUD:-}" ]; then
    warn "ENABLE_POINTCLOUD not set; default is true → pointcloud recorder will try to subscribe to a G1-only topic"
    warn "  Either: source env.go2  (sets ENABLE_POINTCLOUD=false)"
    warn "  Or:     export ENABLE_POINTCLOUD=false"
else
    fail "ENABLE_POINTCLOUD=${ENABLE_POINTCLOUD} but Go2 should disable it"
fi

# LIDAR_ZENOH_TOPIC should be "scan" on Go2
if [ "${LIDAR_ZENOH_TOPIC:-scan}" = "scan" ]; then
    ok "LIDAR_ZENOH_TOPIC=scan"
else
    warn "LIDAR_ZENOH_TOPIC=${LIDAR_ZENOH_TOPIC} (expected 'scan' for Go2; non-default is OK if intentional)"
fi

# LOWSTATE_ZENOH_TOPIC should be rt/lowstate
if [ "${LOWSTATE_ZENOH_TOPIC:-rt/lowstate}" = "rt/lowstate" ]; then
    ok "LOWSTATE_ZENOH_TOPIC=rt/lowstate"
else
    warn "LOWSTATE_ZENOH_TOPIC=${LOWSTATE_ZENOH_TOPIC} (expected 'rt/lowstate')"
fi

# RECORDINGS_DIR
if [ -z "${RECORDINGS_DIR:-}" ]; then
    warn "RECORDINGS_DIR not set (will use ./recordings; change to USB SSD for trip)"
else
    if [ -d "${RECORDINGS_DIR}" ] && [ -w "${RECORDINGS_DIR}" ]; then
        ok "RECORDINGS_DIR=${RECORDINGS_DIR} (writable)"
    else
        fail "RECORDINGS_DIR=${RECORDINGS_DIR} but path not writable / missing"
    fi
fi

hdr "External tools"

for tool in ffmpeg ffprobe ping ros2; do
    if command -v $tool >/dev/null 2>&1; then
        ok "$tool installed ($(command -v $tool))"
    else
        fail "$tool NOT FOUND — required dependency"
    fi
done

hdr "Disk capacity"

target_dir="${RECORDINGS_DIR:-./recordings}"
target_base=$(dirname "$target_dir")
[ -d "$target_base" ] || target_base="/"
avail=$(df -h "$target_base" 2>/dev/null | awk 'NR==2 {print $4}')
avail_gb=$(df -BG "$target_base" 2>/dev/null | awk 'NR==2 {print $4}' | tr -d 'G')
echo "  Target: $target_base"
echo "  Available: $avail"

# Go2 records ~110 GB/day so 500 GB free = ~4 days
if [ -z "$avail_gb" ]; then
    warn "Could not determine disk space"
elif [ "$avail_gb" -ge 500 ]; then
    ok "Disk has ${avail} free — comfortable for road trip (Go2 ~110 GB/day)"
elif [ "$avail_gb" -ge 100 ]; then
    warn "Disk has ${avail} free — OK for a day or two of Go2 recording"
else
    fail "Disk has only ${avail} free — not enough for serious recording"
fi

hdr "Calibration dump (one-time per session)"

if [ -n "${RECORDINGS_DIR:-}" ]; then
    cal_dir="${RECORDINGS_DIR}/$(date +%Y-%m-%d)/calibration"
    if [ -d "$cal_dir" ] && [ -f "$cal_dir/tf_static.yaml" ]; then
        ok "Calibration already dumped at $cal_dir"
    else
        echo "  Recommended: dump latched calibration topics ONCE before recording:"
        echo ""
        echo "     mkdir -p ${cal_dir}"
        echo "     ros2 topic echo --once /tf_static > ${cal_dir}/tf_static.yaml"
        echo "     ros2 topic echo --once /camera/realsense2_camera_node/color/camera_info > ${cal_dir}/color_cam_info.yaml"
        echo "     ros2 topic echo --once /camera/realsense2_camera_node/depth/camera_info > ${cal_dir}/depth_cam_info.yaml"
        echo "     ros2 topic echo --once /camera/realsense2_camera_node/extrinsics/depth_to_color > ${cal_dir}/depth_to_color.yaml"
        echo ""
        echo "  Without these, you cannot project LiDAR onto camera images later."
        WARN=$((WARN+1))
    fi
fi


echo ""
echo "═══════════════════════════════════════════════════════════════"
echo "  Pass: $PASS    Fail: $FAIL    Warn: $WARN"
echo "═══════════════════════════════════════════════════════════════"
if [ $FAIL -gt 0 ]; then
    echo "❌ Critical checks failed — fix before recording."
    exit 1
elif [ $WARN -gt 0 ]; then
    echo "⚠️  All critical checks passed but with warnings — review them above."
    echo "   Most common: ENABLE_POINTCLOUD not explicitly set,"
    echo "   or calibration not yet dumped."
    exit 0
else
    echo "✅ Everything checks out.  Ready to record on Go2."
    exit 0
fi
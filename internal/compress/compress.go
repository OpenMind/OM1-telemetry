// Package compress shrinks recorded session files before upload. zstd
// (whole-file and depth) is lossless, so the original is deleted once
// compression succeeds. Draco (pointcloud) is lossy, so its original is
// kept locally under raw/ -- see rawstore.go -- and never uploaded.
package compress

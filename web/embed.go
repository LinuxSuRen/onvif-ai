// Package web 将前端构建产物（dist）嵌入后端二进制，
// 运行时不再依赖外置的 web/dist 目录。
package web

import "embed"

// Dist 是前端构建产物的嵌入式文件系统，根目录对应 web/dist。
// 构建前请先执行 cd web && npm run build（仓库内置了占位 index.html，
// 未构建前端时 go build 仍可通过，仅页面为占位提示）。
//
//go:embed all:dist
var Dist embed.FS

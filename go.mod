module exe_installer

go 1.24

toolchain go1.24.5

require (
	github.com/go-ole/go-ole v1.3.0 // indirect (for COM shortcut creation)
	golang.org/x/sys v0.27.0
)

require github.com/lxn/walk v0.0.0-20210112085537-c389da54e794

require (
	github.com/lxn/win v0.0.0-20210218163916-a377121e959e // indirect
	gopkg.in/Knetic/govaluate.v3 v3.0.0 // indirect
)

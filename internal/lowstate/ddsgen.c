// Pulls in the idlc-generated type-support translation units (see
// Makefile's idl-gen target) so cgo compiles and links them as part of
// this package — cgo only auto-builds .c files that live alongside the
// package's .go files, not ones in internal/ddsgen/.
#include "unitree_go_lowstate.c"
#include "unitree_hg_lowstate.c"

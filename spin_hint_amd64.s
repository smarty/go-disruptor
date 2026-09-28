#include "textflag.h"

// func spinHint()
//
// PAUSE tells the core it is in a spin-wait loop: it avoids the memory-order mis-speculation (and pipeline flush)
// that a tight load loop suffers when the watched line finally changes, saves power, and yields execution resources
// to the SMT sibling. Eight PAUSEs per call also slow the caller's polling, so a spinning consumer takes the
// producer's cache lines away less often: on an i7-12700K, one PAUSE left the single writer ~40% slower than the
// default wait strategy, and eight matched it, while adding only ~100-250ns of wake-up latency.
TEXT ·spinHint(SB), NOSPLIT, $0-0
	MOVL $8, AX
pause:
	PAUSE
	DECL AX
	JNZ pause
	RET

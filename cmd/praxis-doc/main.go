// praxis-doc prints the praxis issue standard, generated from internal/praxis, to stdout.
package main

import (
	"fmt"

	"github.com/gluck/ebac/internal/praxis"
)

func main() { fmt.Print(praxis.GenDoc()) }

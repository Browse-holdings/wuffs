// Use of this source code is governed by a BSD-style license that can be found
// in the LICENSE file.

//go:generate go run gen.go

// puffs-gen-c transpiles a Puffs program to a C program.
//
// The command line arguments list the source Puffs files. If no arguments are
// given, it reads from stdin.
//
// The generated program is written to stdout.
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/google/puffs/lang/generate"

	a "github.com/google/puffs/lang/ast"
	t "github.com/google/puffs/lang/token"
)

func main() {
	generate.Main(func(pkgName string, idMap *t.IDMap, files []*a.File) ([]byte, error) {
		g := &gen{
			pkgName: pkgName,
			idMap:   idMap,
			files:   files,
		}
		if err := g.generate(); err != nil {
			return nil, err
		}
		stdout := &bytes.Buffer{}
		cmd := exec.Command("clang-format", "-style=Chromium")
		cmd.Stdin = &g.buffer
		cmd.Stdout = stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return nil, err
		}
		return stdout.Bytes(), nil
	})
}

var builtInStatuses = [...]string{
	// For API/ABI compatibility, the very first two status values must be
	// status_ok (with generated value 0) and error_bad_version (with generated
	// value -2 + 1). This lets caller code check the constructor return value
	// for error_bad_version even if the caller and callee were built with
	// different versions.
	"status_ok",
	"error_bad_version",
	// The order of the remaining status values is less important.
	"error_bad_receiver",
	"error_bad_argument",
	"error_constructor_not_called",
	"status_short_dst",
	"status_short_src",
}

type visibility uint32

const (
	bothPubPri visibility = iota
	pubOnly
	priOnly
)

type gen struct {
	buffer      bytes.Buffer
	pkgName     string
	idMap       *t.IDMap
	files       []*a.File
	jumpTargets map[*a.While]uint32
}

func (g *gen) printf(format string, args ...interface{}) { fmt.Fprintf(&g.buffer, format, args...) }
func (g *gen) writeb(b byte)                             { g.buffer.WriteByte(b) }
func (g *gen) writes(s string)                           { g.buffer.WriteString(s) }

func (g *gen) jumpTarget(n *a.While) (uint32, error) {
	if g.jumpTargets == nil {
		g.jumpTargets = map[*a.While]uint32{}
	}
	if jt, ok := g.jumpTargets[n]; ok {
		return jt, nil
	}
	jt := uint32(len(g.jumpTargets))
	if jt == 1000000 {
		return 0, fmt.Errorf("too many jump targets")
	}
	g.jumpTargets[n] = jt
	return jt, nil
}

func (g *gen) generate() error {
	includeGuard := "PUFFS_" + strings.ToUpper(g.pkgName) + "_H"
	g.printf("#ifndef %s\n#define %s\n\n", includeGuard, includeGuard)

	g.printf("// Code generated by puffs-gen-c. DO NOT EDIT.\n\n")
	g.writes(baseHeader)
	g.writes("\n#ifdef __cplusplus\nextern \"C\" {\n#endif\n\n")

	g.writes("// ---------------- Status Codes\n\n")
	g.writes("// Status codes are non-positive integers.\n")
	g.writes("//\n")
	g.writes("// The least significant bit indicates a non-recoverable status code: an error.\n")
	g.writes("typedef enum {\n")
	for i, s := range builtInStatuses {
		nudge := ""
		if strings.HasPrefix(s, "error_") {
			nudge = "+1"
		}
		g.printf("puffs_%s_%s = %d%s,\n", g.pkgName, s, -2*i, nudge)
	}
	g.printf("} puffs_%s_status;\n\n", g.pkgName)
	g.printf("bool puffs_%s_status_is_error(puffs_%s_status s);\n\n", g.pkgName, g.pkgName)
	g.printf("const char* puffs_%s_status_string(puffs_%s_status s);\n\n", g.pkgName, g.pkgName)

	g.writes("// ---------------- Public Structs\n\n")
	if err := g.forEachStruct(pubOnly, (*gen).writeStruct); err != nil {
		return err
	}

	g.writes("// ---------------- Public Constructor and Destructor Prototypes\n\n")
	if err := g.forEachStruct(pubOnly, (*gen).writeCtorPrototypesPub); err != nil {
		return err
	}

	g.writes("// ---------------- Public Function Prototypes\n\n")
	if err := g.forEachFunc(pubOnly, (*gen).writeFuncPrototype); err != nil {
		return err
	}

	// Finish up the header, which is also the first part of the .c file.
	g.writes("\n#ifdef __cplusplus\n}  // extern \"C\"\n#endif\n\n")
	g.printf("#endif  // %s\n\n", includeGuard)
	g.writes("// C HEADER ENDS HERE.\n\n")
	g.writes(baseImpl)
	g.writes("\n")

	g.writes("// ---------------- Status Codes Implementations\n\n")
	g.printf("bool puffs_%s_status_is_error(puffs_%s_status s) {"+
		"return s & 1; }\n\n", g.pkgName, g.pkgName)
	g.printf("const char* puffs_%s_status_strings[%d] = {\n", g.pkgName, len(builtInStatuses))
	for _, s := range builtInStatuses {
		if strings.HasPrefix(s, "status_") {
			s = s[len("status_"):]
		} else if strings.HasPrefix(s, "error_") {
			s = s[len("error_"):]
		}
		s = g.pkgName + ": " + strings.Replace(s, "_", " ", -1)
		g.printf("%q,", s)
	}
	g.writes("};\n\n")
	g.printf("const char* puffs_%s_status_string(puffs_%s_status s) {\n", g.pkgName, g.pkgName)
	g.writes("s = -(s >> 1);")
	g.printf("if ((0 <= s) && (s < %d)) { return puffs_%s_status_strings[s]; }\n",
		len(builtInStatuses), g.pkgName)
	g.writes("return \"\";\n")
	g.writes("}\n\n")

	g.writes("// ---------------- Private Structs\n\n")
	if err := g.forEachStruct(priOnly, (*gen).writeStruct); err != nil {
		return err
	}

	g.writes("// ---------------- Private Constructor and Destructor Prototypes\n\n")
	if err := g.forEachStruct(priOnly, (*gen).writeCtorPrototypesPri); err != nil {
		return err
	}

	g.writes("// ---------------- Private Function Prototypes\n\n")
	if err := g.forEachFunc(priOnly, (*gen).writeFuncPrototype); err != nil {
		return err
	}

	g.writes("// ---------------- Constructor and Destructor Implementations\n\n")
	g.writes("// PUFFS_MAGIC is a magic number to check that constructors are called. It's\n")
	g.writes("// not foolproof, given C doesn't automatically zero memory before use, but it\n")
	g.writes("// should catch 99.99% of cases.\n")
	g.writes("//\n")
	g.writes("// Its (non-zero) value is arbitrary, based on md5sum(\"puffs\").\n")
	g.writes("#define PUFFS_MAGIC (0xCB3699CCU)\n\n")
	g.writes("// PUFFS_ALREADY_ZEROED is passed from a container struct's constructor to a\n")
	g.writes("// containee struct's constructor when the container has already zeroed the\n")
	g.writes("// containee's memory.\n")
	g.writes("//\n")
	g.writes("// Its (non-zero) value is arbitrary, based on md5sum(\"zeroed\").\n")
	g.writes("#define PUFFS_ALREADY_ZEROED (0x68602EF1U)\n\n")
	if err := g.forEachStruct(bothPubPri, (*gen).writeCtorImpls); err != nil {
		return err
	}

	g.writes("// ---------------- Function Implementations\n\n")
	if err := g.forEachFunc(bothPubPri, (*gen).writeFuncImpl); err != nil {
		return err
	}

	return nil
}

func (g *gen) forEachFunc(v visibility, f func(*gen, *a.Func) error) error {
	for _, file := range g.files {
		for _, n := range file.TopLevelDecls() {
			if n.Kind() != a.KFunc ||
				(v == pubOnly && n.Raw().Flags()&a.FlagsPublic == 0) ||
				(v == priOnly && n.Raw().Flags()&a.FlagsPublic != 0) {
				continue
			}
			if err := f(g, n.Func()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (g *gen) forEachStruct(v visibility, f func(*gen, *a.Struct) error) error {
	for _, file := range g.files {
		for _, n := range file.TopLevelDecls() {
			if n.Kind() != a.KStruct ||
				(v == pubOnly && n.Raw().Flags()&a.FlagsPublic == 0) ||
				(v == priOnly && n.Raw().Flags()&a.FlagsPublic != 0) {
				continue
			}
			if err := f(g, n.Struct()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (g *gen) writeStruct(n *a.Struct) error {
	// For API/ABI compatibility, the very first field in the struct must be
	// the status code. This lets the constructor callee set "this->status =
	// etc_error_bad_version;" regardless of the sizeof(*this) struct reserved
	// by the caller and even if the caller and callee were built with
	// different versions.
	structName := n.Name().String(g.idMap)
	g.printf("typedef struct {\n")
	if n.Suspendible() {
		g.printf("puffs_%s_status status;\n", g.pkgName)
		g.printf("uint32_t magic;\n")
	}
	for _, f := range n.Fields() {
		if err := g.writeField(f.Field(), "f_"); err != nil {
			return err
		}
		g.writes(";\n")
	}
	g.printf("} puffs_%s_%s;\n\n", g.pkgName, structName)
	return nil
}

func (g *gen) writeCtorSignature(n *a.Struct, public bool, ctor bool) {
	structName := n.Name().String(g.idMap)
	ctorName := "destructor"
	if ctor {
		ctorName = "constructor"
		if public {
			g.printf("// puffs_%s_%s_%s is a constructor function.\n", g.pkgName, structName, ctorName)
			g.printf("//\n")
			g.printf("// It should be called before any other puffs_%s_%s_* function.\n",
				g.pkgName, structName)
			g.printf("//\n")
			g.printf("// Pass PUFFS_VERSION and 0 for puffs_version and for_internal_use_only.\n")
		}
	}
	g.printf("void puffs_%s_%s_%s(puffs_%s_%s *self", g.pkgName, structName, ctorName, g.pkgName, structName)
	if ctor {
		g.printf(", uint32_t puffs_version, uint32_t for_internal_use_only")
	}
	g.printf(")")
}

func (g *gen) writeCtorPrototypesPub(n *a.Struct) error {
	return g.writeCtorPrototypes(n, true)
}

func (g *gen) writeCtorPrototypesPri(n *a.Struct) error {
	return g.writeCtorPrototypes(n, false)
}

func (g *gen) writeCtorPrototypes(n *a.Struct, public bool) error {
	if !n.Suspendible() {
		return nil
	}
	for _, ctor := range []bool{true, false} {
		g.writeCtorSignature(n, public, ctor)
		g.writes(";\n\n")
	}
	return nil
}

func (g *gen) writeCtorImpls(n *a.Struct) error {
	if !n.Suspendible() {
		return nil
	}
	for _, ctor := range []bool{true, false} {
		g.writeCtorSignature(n, false, ctor)
		g.printf("{\n")
		g.printf("if (!self) { return; }\n")

		if ctor {
			g.printf("if (puffs_version != PUFFS_VERSION) {\n")
			g.printf("self->status = puffs_%s_error_bad_version;\n", g.pkgName)
			g.printf("return;\n")
			g.printf("}\n")

			g.writes("if (for_internal_use_only != PUFFS_ALREADY_ZEROED) {" +
				"memset(self, 0, sizeof(*self)); }\n")
			g.writes("self->magic = PUFFS_MAGIC;\n")

			for _, f := range n.Fields() {
				f := f.Field()
				if dv := f.DefaultValue(); dv != nil {
					// TODO: set default values for array types.
					g.printf("self->f_%s = %d;\n", f.Name().String(g.idMap), dv.ConstValue())
				}
			}
		}

		// TODO: call any ctor/dtors on sub-structures.
		g.writes("}\n\n")
	}
	return nil
}

func (g *gen) writeFuncSignature(n *a.Func) error {
	// TODO: write n's return values.
	if n.Suspendible() {
		g.printf("puffs_%s_status", g.pkgName)
	} else {
		g.printf("void")
	}
	g.printf(" puffs_%s", g.pkgName)
	if r := n.Receiver(); r != 0 {
		g.printf("_%s", r.String(g.idMap))
	}
	g.printf("_%s(", n.Name().String(g.idMap))

	comma := false
	if r := n.Receiver(); r != 0 {
		g.printf("puffs_%s_%s *self", g.pkgName, r.String(g.idMap))
		comma = true
	}
	for _, o := range n.In().Fields() {
		if comma {
			g.writeb(',')
		}
		comma = true
		if err := g.writeField(o.Field(), "a_"); err != nil {
			return err
		}
	}

	g.printf(")")
	return nil
}

func (g *gen) writeFuncPrototype(n *a.Func) error {
	if err := g.writeFuncSignature(n); err != nil {
		return err
	}
	g.writes(";\n\n")
	return nil
}

func (g *gen) writeFuncImpl(n *a.Func) error {
	g.jumpTargets = nil
	if err := g.writeFuncSignature(n); err != nil {
		return err
	}
	g.writes("{\n")

	cleanup0 := false

	// Check the previous status and the args.
	if n.Public() {
		if n.Receiver() != 0 {
			g.printf("if (!self) { return puffs_%s_error_bad_receiver; }\n", g.pkgName)
		}
	}
	if n.Suspendible() {
		g.printf("puffs_%s_status status = ", g.pkgName)
		if n.Receiver() != 0 {
			g.printf("self->status;\n")
			if n.Public() {
				g.printf("if (status & 1) { return status; }")
			}
		} else {
			g.printf("puffs_%s_status_ok;\n", g.pkgName)
		}
		if n.Public() {
			g.printf("if (self->magic != PUFFS_MAGIC) {"+
				"status = puffs_%s_error_constructor_not_called; goto cleanup0; }\n", g.pkgName)
			cleanup0 = true
		}
	} else if r := n.Receiver(); r != 0 {
		// TODO: fix this.
		return fmt.Errorf(`cannot convert Puffs function "%s.%s" to C`,
			r.String(g.idMap), n.Name().String(g.idMap))
	}
	if n.Public() {
		badArg := false
		for _, o := range n.In().Fields() {
			o := o.Field()
			if o.XType().PackageOrDecorator().Key() != t.KeyPtr {
				// TODO: check for type refinements: u32[..4095] instead of
				// u32. Also check for types, for array-typed arguments.
				continue
			}
			if badArg {
				g.writes(" || ")
			} else {
				g.writes("if (")
			}
			g.printf("!a_%s", o.Name().String(g.idMap))
			badArg = true
		}
		if badArg {
			g.writes(") {")
			if n.Suspendible() {
				g.printf("status = puffs_%s_error_bad_argument; goto cleanup0; }\n", g.pkgName)
			} else {
				g.printf("return puffs_%s_error_bad_argument; }\n", g.pkgName)
			}
		}
	}
	g.writes("\n")

	// Generate the local variables.
	if err := g.writeVars(n.Node(), 0); err != nil {
		return err
	}
	g.writes("\n")

	// Generate the function body.
	for _, o := range n.Body() {
		if err := g.writeStatement(o, 0); err != nil {
			return err
		}
	}
	g.writes("\n")

	if cleanup0 {
		g.printf("cleanup0: self->status = status;\n")
	}
	if n.Suspendible() {
		g.printf("return status;\n")
	}

	g.writes("}\n\n")
	return nil
}

func (g *gen) writeField(n *a.Field, namePrefix string) error {
	const maxNPtr = 16

	convertible, nPtr := true, 0
	for x := n.XType(); x != nil; x = x.Inner() {
		if p := x.PackageOrDecorator().Key(); p == t.KeyPtr {
			if nPtr == maxNPtr {
				return fmt.Errorf("cannot convert Puffs type %q to C: too many ptr's", n.XType().String(g.idMap))
			}
			nPtr++
			continue
		} else if p == t.KeyOpenBracket {
			continue
		} else if p != 0 {
			convertible = false
			break
		}
		if k := x.Name().Key(); k < t.Key(len(cTypeNames)) {
			if s := cTypeNames[k]; s != "" {
				g.writes(s)
				g.writeb(' ')
				continue
			}
		}
		convertible = false
		break
	}
	if !convertible {
		// TODO: fix this.
		return fmt.Errorf("cannot convert Puffs type %q to C", n.XType().String(g.idMap))
	}

	for i := 0; i < nPtr; i++ {
		g.writeb('*')
	}
	g.writes(namePrefix)
	g.writes(n.Name().String(g.idMap))

	for x := n.XType(); x != nil; x = x.Inner() {
		if x.PackageOrDecorator() == t.IDOpenBracket {
			g.writeb('[')
			g.writes(x.ArrayLength().ConstValue().String())
			g.writeb(']')
		}
	}

	return nil
}

func (g *gen) writeVars(n *a.Node, depth uint32) error {
	if depth > a.MaxBodyDepth {
		return fmt.Errorf("body recursion depth too large")
	}
	depth++

	if n.Kind() == a.KVar {
		x := n.Var().XType()
		if k := x.Name().Key(); k < t.Key(len(cTypeNames)) {
			if s := cTypeNames[k]; s != "" {
				g.printf("%s v_%s;\n", s, n.Var().Name().String(g.idMap))
				return nil
			}
		}
		// TODO: fix this.
		return fmt.Errorf("cannot convert Puffs type %q to C", x.String(g.idMap))
	}

	for _, l := range n.Raw().SubLists() {
		for _, o := range l {
			if err := g.writeVars(o, depth); err != nil {
				return err
			}
		}
	}
	return nil
}

func (g *gen) writeStatement(n *a.Node, depth uint32) error {
	if depth > a.MaxBodyDepth {
		return fmt.Errorf("body recursion depth too large")
	}
	depth++

	switch n.Kind() {
	case a.KAssert:
		// Assertions only apply at compile-time.
		return nil

	case a.KAssign:
		n := n.Assign()
		if err := g.writeExpr(n.LHS(), depth); err != nil {
			return err
		}
		// TODO: does KeyAmpHatEq need special consideration?
		g.writes(cOpNames[0xFF&n.Operator().Key()])
		if err := g.writeExpr(n.RHS(), depth); err != nil {
			return err
		}
		g.writes(";\n")
		return nil

	case a.KIf:
		// TODO.

	case a.KJump:
		n := n.Jump()
		jt := g.jumpTargets[n.JumpTarget()]
		keyword := "continue"
		if n.Keyword().Key() == t.KeyBreak {
			keyword = "break"
		}
		g.printf("goto label_%d_%s;\n", jt, keyword)
		return nil

	case a.KReturn:
		// TODO.

	case a.KVar:
		n := n.Var()
		g.printf("v_%s = ", n.Name().String(g.idMap))
		if v := n.Value(); v != nil {
			if err := g.writeExpr(v, 0); err != nil {
				return err
			}
		} else {
			g.writeb('0')
		}
		g.writes(";\n")
		return nil

	case a.KWhile:
		n := n.While()

		if n.HasContinue() {
			jt, err := g.jumpTarget(n)
			if err != nil {
				return err
			}
			g.printf("label_%d_continue:;\n", jt)
		}
		g.writes("while (")
		if err := g.writeExpr(n.Condition(), 0); err != nil {
			return err
		}
		g.writes(") {\n")
		for _, o := range n.Body() {
			if err := g.writeStatement(o, depth); err != nil {
				return err
			}
		}
		g.writes("}\n")
		if n.HasBreak() {
			jt, err := g.jumpTarget(n)
			if err != nil {
				return err
			}
			g.printf("label_%d_break:;\n", jt)
		}
		return nil

	}
	return fmt.Errorf("unrecognized ast.Kind (%s) for writeStatement", n.Kind())
}

func (g *gen) writeExpr(n *a.Expr, depth uint32) error {
	if depth > a.MaxExprDepth {
		return fmt.Errorf("expression recursion depth too large")
	}
	depth++

	if cv := n.ConstValue(); cv != nil {
		// TODO: write false/true instead of 0/1 if n.MType() is bool?
		g.writes(cv.String())
		return nil
	}

	switch n.ID0().Flags() & (t.FlagsUnaryOp | t.FlagsBinaryOp | t.FlagsAssociativeOp) {
	case 0:
		if err := g.writeExprOther(n, depth); err != nil {
			return err
		}
	case t.FlagsUnaryOp:
		if err := g.writeExprUnaryOp(n, depth); err != nil {
			return err
		}
	case t.FlagsBinaryOp:
		if err := g.writeExprBinaryOp(n, depth); err != nil {
			return err
		}
	case t.FlagsAssociativeOp:
		if err := g.writeExprAssociativeOp(n, depth); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unrecognized token.Key (0x%X) for writeExpr", n.ID0().Key())
	}

	return nil
}

func (g *gen) writeExprOther(n *a.Expr, depth uint32) error {
	switch n.ID0().Key() {
	case 0:
		if id1 := n.ID1(); id1.Key() == t.KeyThis {
			g.writes("self")
		} else {
			// TODO: don't assume that the v_ prefix is necessary.
			g.writes("v_")
			g.writes(id1.String(g.idMap))
		}
		return nil

	case t.KeyOpenParen:
	// n is a function call.
	// TODO.

	case t.KeyOpenBracket:
	// n is an index.
	// TODO.

	case t.KeyColon:
	// n is a slice.
	// TODO.

	case t.KeyDot:
		if err := g.writeExpr(n.LHS().Expr(), depth); err != nil {
			return err
		}
		// TODO: choose between . vs -> operators.
		//
		// TODO: don't assume that the f_ prefix is necessary.
		g.writes("->f_")
		g.writes(n.ID1().String(g.idMap))
		return nil
	}
	return fmt.Errorf("unrecognized token.Key (0x%X) for writeExprOther", n.ID0().Key())
}

func (g *gen) writeExprUnaryOp(n *a.Expr, depth uint32) error {
	// TODO.
	return nil
}

func (g *gen) writeExprBinaryOp(n *a.Expr, depth uint32) error {
	op := n.ID0()
	if op.Key() == t.KeyXBinaryAs {
		// TODO.
		return nil
	}
	g.writeb('(')
	if err := g.writeExpr(n.LHS().Expr(), depth); err != nil {
		return err
	}
	// TODO: does KeyXBinaryAmpHat need special consideration?
	g.writes(cOpNames[0xFF&op.Key()])
	if err := g.writeExpr(n.RHS().Expr(), depth); err != nil {
		return err
	}
	g.writeb(')')
	return nil
}

func (g *gen) writeExprAssociativeOp(n *a.Expr, depth uint32) error {
	// TODO.
	return nil
}

var cTypeNames = [...]string{
	t.KeyI8:    "int8_t",
	t.KeyI16:   "int16_t",
	t.KeyI32:   "int32_t",
	t.KeyI64:   "int64_t",
	t.KeyU8:    "uint8_t",
	t.KeyU16:   "uint16_t",
	t.KeyU32:   "uint32_t",
	t.KeyU64:   "uint64_t",
	t.KeyUsize: "size_t",
	t.KeyBool:  "bool",
	t.KeyBuf1:  "puffs_base_buf1",
	t.KeyBuf2:  "puffs_base_buf2",
}

var cOpNames = [256]string{
	t.KeyEq:       " = ",
	t.KeyPlusEq:   " += ",
	t.KeyMinusEq:  " -= ",
	t.KeyStarEq:   " *= ",
	t.KeySlashEq:  " /= ",
	t.KeyShiftLEq: " <<= ",
	t.KeyShiftREq: " >>= ",
	t.KeyAmpEq:    " &= ",
	t.KeyAmpHatEq: " no_such_amp_hat_C_operator ",
	t.KeyPipeEq:   " |= ",
	t.KeyHatEq:    " ^= ",

	t.KeyXUnaryPlus:  "+",
	t.KeyXUnaryMinus: "-",
	t.KeyXUnaryNot:   "!",

	t.KeyXBinaryPlus:        " + ",
	t.KeyXBinaryMinus:       " - ",
	t.KeyXBinaryStar:        " * ",
	t.KeyXBinarySlash:       " / ",
	t.KeyXBinaryShiftL:      " << ",
	t.KeyXBinaryShiftR:      " >> ",
	t.KeyXBinaryAmp:         " & ",
	t.KeyXBinaryAmpHat:      " no_such_amp_hat_C_operator ",
	t.KeyXBinaryPipe:        " | ",
	t.KeyXBinaryHat:         " ^ ",
	t.KeyXBinaryNotEq:       " != ",
	t.KeyXBinaryLessThan:    " < ",
	t.KeyXBinaryLessEq:      " <= ",
	t.KeyXBinaryEqEq:        " == ",
	t.KeyXBinaryGreaterEq:   " >= ",
	t.KeyXBinaryGreaterThan: " > ",
	t.KeyXBinaryAnd:         " && ",
	t.KeyXBinaryOr:          " || ",
	t.KeyXBinaryAs:          " no_such_as_C_operator ",

	t.KeyXAssociativePlus: " + ",
	t.KeyXAssociativeStar: " * ",
	t.KeyXAssociativeAmp:  " & ",
	t.KeyXAssociativePipe: " | ",
	t.KeyXAssociativeHat:  " ^ ",
	t.KeyXAssociativeAnd:  " && ",
	t.KeyXAssociativeOr:   " || ",
}

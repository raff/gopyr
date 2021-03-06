package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strings"

	"github.com/go-python/gpython/ast"
	"github.com/go-python/gpython/parser"
	"github.com/go-python/gpython/py"

	"github.com/raff/jennifer/jen"
)

var (
	debugLevel   int
	panicUnknown bool
	verbose      bool
	lineno       bool
	mainpackage  bool

	gokeywords = map[string]string{
		// Convert python names to pygor names
		"str":     "string",
		"float":   "float64",
		"complex": "complex128",

		// XXX: these should actually be converted to runtime.Dict, runtime.List...
		// and renamed if used as "attributes" (i.e. self.dict) or parameter name
		"dict":  "Dict",
		"list":  "List",
		"tuple": "Tuple",

		// these are not go keywords but they are used by pygor
		"Any":   "AnyΠ",
		"Dict":  "DictΠ",
		"List":  "ListΠ",
		"Tuple": "TupleΠ",

		// these are standard package names we may want to preserve
		"fmt": "fmtΠ",

		// these are go keywords that need to be renamed
		"case":        "caseΠ",
		"chan":        "chanΠ",
		"const":       "constΠ",
		"default":     "defaultΠ",
		"defer":       "deferΠ",
		"fallthrough": "fallthroughΠ",
		"func":        "funcΠ",
		"go":          "goΠ",
		"goto":        "gotoΠ",
		"interface":   "interfaceΠ",
		"map":         "mapΠ",
		"package":     "packageΠ",
		"range":       "rangeΠ",
		"select":      "selectΠ",
		"struct":      "structΠ",
		"switch":      "switchΠ",
		"type":        "typeΠ",
		"var":         "varΠ",
	}

	goRuntime = "github.com/raff/pygor/runtime"

	goAny             = jen.Qual(goRuntime, "Any")
	goList            = jen.Qual(goRuntime, "List")
	goTuple           = jen.Qual(goRuntime, "Tuple")
	goDict            = jen.Qual(goRuntime, "Dict")
	goAssert          = jen.Qual(goRuntime, "Assert")
	goContains        = jen.Qual(goRuntime, "Contains")
	goException       = jen.Qual(goRuntime, "PyException")
	goRaisedException = jen.Qual(goRuntime, "RaisedException")
)

func rename(s string) string {
	if n, ok := gokeywords[s]; ok {
		return n
	}

	return s
}

func renameId(id ast.Identifier) string {
	return rename(string(id))
}

func unknown(typ string, v interface{}) *jen.Statement {
	msg := fmt.Sprintf("UNKNOWN-%v: %T %#v", typ, v, v)

	if expr, ok := v.(ast.Expr); ok {
		msg += fmt.Sprintf(" at line %d, col %d", expr.GetLineno(), expr.GetColOffset())
	}

	if panicUnknown {
		panic(msg)
	}

	return jen.Lit(msg)
}

func trimlines(s py.String) string {
	var lines []string

	for _, l := range strings.Split(string(s), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}

	return strings.Join(lines, "\n")
}

type ScopeReturn int

const (
	ReturnNoFunction ScopeReturn = iota
	ReturnNone
	ReturnReturn
	ReturnYield
)

func (r ScopeReturn) String() string {
	switch r {
	case ReturnNoFunction:
		return "NotAFunction"
	case ReturnNone:
		return "None"
	case ReturnReturn:
		return "Return"
	case ReturnYield:
		return "Yield"
	}

	return "UNKNOWN"
}

type Scope struct {
	level   int // nesting level
	vars    map[string]struct{}
	imports map[string]string
	main    bool

	file *jen.File

	parsed  *jen.Statement
	body    []*jen.Statement
	methods []*jen.Statement

	returnType ScopeReturn

	next *Scope
	prev *Scope
}

func NewScope(f *jen.File, imp ...map[string]string) *Scope {
	scope := &Scope{vars: make(map[string]struct{}), parsed: jen.Null(), file: f}
	if len(imp) > 0 {
		scope.imports = imp[0]
	} else {
		scope.imports = make(map[string]string)
	}

	return scope
}

func (s *Scope) Render() (parsed *jen.Statement) {
	// this is done in order to generate the correct dependencies
	// since the render fails on certaing blocks
	//
	// the real rendering is done at the end,
	// formatting the list of statements one at a time
	s.file.Group = &jen.Group{}
	s.file.Add(s.parsed)
	if err := s.file.Render(ioutil.Discard); err != nil {
		//log.Println("RENDER", err)
	}

	parsed, s.parsed = s.parsed, jen.Null()
	return
}

func (s *Scope) Top() bool {
	return s.prev == nil
}

func (s *Scope) Push() *Scope {
	s.next = NewScope(s.file, s.imports)
	s.next.prev = s
	s.next.level = s.level + 1
	if verbose {
		log.Println("PUSH", s.next.level)
	}
	return s.next
}

func (s *Scope) Pop(popret bool) *Scope {
	s.parsed = nil
	s.prev.next = nil
	if !popret {
		s.prev.returnType = s.returnType
	}
	if s.methods != nil {
		s.prev.methods = append(s.prev.methods, s.methods...)
		s.methods = nil
	}
	if s.prev.prev == nil && s.prev.methods != nil {
		s.prev.body = append(s.prev.body, s.prev.methods...)
		s.prev.methods = nil
	}
	if verbose {
		log.Println("POP", s.prev.level)
	}
	return s.prev
}

func (s *Scope) Add(stmt *jen.Statement) {
	if verbose {
		log.Printf("GGG %#v\n", stmt)
	}

	s.parsed.Add(stmt)
	s.body = append(s.body, stmt)
}

// check if the element in the expression list are new names
// (and add them to the list of known names)
func (s *Scope) newNames(lexpr []ast.Expr) (ret bool) {
	for _, x := range lexpr {
		var nn string

		switch t := x.(type) {
		case *ast.Name:
			nn = string(t.Id)

		default:
			continue
		}

		// if we have seen the name before, in any scope,
		// it's defined. Otherwise "define" it in the current scope.
		// (but if forceNew is set, these are new names in the scope)
		found := false

		for curr := s; curr != nil; curr = curr.prev {
			if _, ok := curr.vars[nn]; ok {
				found = true
				break
			}
		}

		if !found {
			s.vars[nn] = struct{}{}
			ret = true
		}
	}

	return
}

func (s *Scope) addName(id ast.Identifier) {
	s.vars[string(id)] = struct{}{}
}

func (s *Scope) goBoolOp(op ast.BoolOpNumber) *jen.Statement {
	switch op {
	case ast.And:
		return jen.Op("&&")

	case ast.Or:
		return jen.Op("||")
	}

	return unknown("BOOLOP", op.String())
}

func (s *Scope) goUnary(op ast.UnaryOpNumber) *jen.Statement {
	switch op {
	case ast.Not:
		return jen.Op("!")

	case ast.UAdd:
		return jen.Op("+")

	case ast.USub:
		return jen.Op("-")
	}

	return unknown("UNARY", op.String())
}

func (s *Scope) goOp(op ast.OperatorNumber) *jen.Statement {
	return s.goOpExt(op, "")
}

func (s *Scope) goOpExt(op ast.OperatorNumber, ext string) *jen.Statement {
	switch op {
	case ast.Add:
		return jen.Op("+" + ext)
	case ast.Sub:
		return jen.Op("-" + ext)
	case ast.Mult:
		return jen.Op("*" + ext)
	case ast.Div:
		return jen.Op("/" + ext)
	case ast.Modulo:
		return jen.Op("%" + ext)
	case ast.Pow:
		return jen.Op("**" + ext)
	case ast.LShift:
		return jen.Op("<<" + ext)
	case ast.RShift:
		return jen.Op(">>" + ext)
	case ast.BitOr:
		return jen.Op("|" + ext)
	case ast.BitXor:
		return jen.Op("^" + ext)
	case ast.BitAnd:
		return jen.Op("&" + ext)
	case ast.FloorDiv:
		return jen.Op("/ /*floor*/" + ext)
	}

	return unknown("OP", op.String()+ext)
}

func (s *Scope) goCmpOp(op ast.CmpOp) *jen.Statement {
	switch op {
	case ast.Eq:
		return jen.Op("==")
	case ast.NotEq:
		return jen.Op("!=")
	case ast.Lt:
		return jen.Op("<")
	case ast.LtE:
		return jen.Op("<=")
	case ast.Gt:
		return jen.Op(">")
	case ast.GtE:
		return jen.Op(">=")
	case ast.Is:
		return jen.Op("==") // is
	case ast.IsNot:
		return jen.Op("!=") // is not
	case ast.In:
		return jen.Op("in")
	case ast.NotIn:
		return jen.Op("not in")
	}

	return unknown("CMPOP", op.String())
}

func (s *Scope) goSlice(name ast.Expr, value ast.Slicer) *jen.Statement {
	stmt := s.goExpr(name)
	start := jen.Empty()
	end := jen.Empty()

	exprval := func(name, val ast.Expr) *jen.Statement {
		if unary, ok := val.(*ast.UnaryOp); ok && unary.Op == ast.USub { // -x
			return jen.Len(s.goExpr(name)).Op("-").Add(s.goExpr(unary.Operand))
		} else {
			return s.goExpr(val)
		}
	}

	switch sl := value.(type) {
	case *ast.Slice:
		if sl.Lower != nil {
			start = exprval(name, sl.Lower)
		}
		if sl.Upper != nil {
			end = exprval(name, sl.Upper)
		}
		if sl.Step != nil {
			// if sl.Lower==nil && sl.Upper==nil && sl.Step == -1
			// it would be a reverse slice, not that we can easily do it

			log.Printf("at %v:%v", value.GetLineno(), value.GetColOffset())
			panic("step index not implemented")
		}
		stmt.Add(jen.Index(start, end))

	case *ast.Index:
		stmt.Add(jen.Index(exprval(name, sl.Value)))

	case *ast.ExtSlice: // start:stop:step
		log.Printf("at %v:%v", value.GetLineno(), value.GetColOffset())
		panic("ExtSlice not implemented")
	}

	return stmt
}

func (s *Scope) goIdentifiers(l []ast.Identifier) *jen.Statement {
	return jen.ListFunc(func(g *jen.Group) {
		for _, i := range l {
			g.Add(goId(i))
		}
	})
}

func (s *Scope) strIdentifiers(l []ast.Identifier) string {
	var sx []string
	for _, i := range l {
		sx = append(sx, string(i))
	}

	return strings.Join(sx, ",")
}

func (s *Scope) goInitialized(otype *jen.Statement, values []ast.Expr) *jen.Statement {
	return jen.Parens(otype.Clone().ValuesFunc(func(g *jen.Group) {
		for _, v := range values {
			g.Add(s.goExpr(v))
		}
	}))
}

func (s *Scope) goExprList(values []ast.Expr) *jen.Statement {
	return jen.ListFunc(func(g *jen.Group) {
		for _, v := range values {
			g.Add(s.goExpr(v))
		}
	})
}

func (s *Scope) strExprList(values []ast.Expr) string {
	var sx []string
	for _, v := range values {
		sx = append(sx, s.goExpr(v).GoString())
	}

	return strings.Join(sx, ",")
}

func (s *Scope) goExprOrList(expr ast.Expr) *jen.Statement {
	if tuple, ok := expr.(*ast.Tuple); ok {
		return s.goExpr(tuple.Elts)
	}
	return s.goExpr(expr)
}

func lenExpr(expr ast.Expr) int {
	if tuple, ok := expr.(*ast.Tuple); ok {
		return len(tuple.Elts)
	}

	return 1
}

func isNone(expr ast.Expr) bool {
	if c, ok := expr.(*ast.NameConstant); ok {
		return c.Value == py.None
	}

	return false
}

func isTuple(expr ast.Expr) bool {
	_, ok := expr.(*ast.Tuple)
	return ok
}

func isList(expr ast.Expr) bool {
	_, ok := expr.(*ast.List)
	return ok
}

// check for `__name__ == "__main__"`
func isNameMain(expr ast.Expr) bool {
	comp, ok := expr.(*ast.Compare)
	if !ok {
		return false
	}
	if len(comp.Ops) != 1 {
		return false
	}
	if comp.Ops[0] != ast.Eq {
		return false
	}

	name, ok := comp.Left.(*ast.Name)
	if !ok {
		return false
	}
	if string(name.Id) != "__name__" {
		return false
	}

	str, ok := comp.Comparators[0].(*ast.Str)
	if !ok {
		return false
	}
	if string(str.S) != "__main__" {
		return false
	}

	return true
}

func exprIds(expr ast.Expr) (ids []ast.Identifier) {
	if tuple, ok := expr.(*ast.Tuple); ok {
		for _, x := range tuple.Elts {
			ids = append(ids, x.(*ast.Name).Id)
		}
	} else {
		ids = append(ids, expr.(*ast.Name).Id)
	}

	return
}

func (s *Scope) gomprehension(c ast.Comprehension) (*jen.Statement, *jen.Statement) {
	iter, _ := s.goFor(c.Target, c.Iter)
	cond := iter
	if len(c.Ifs) > 0 {
		ccond := s.goExpr(c.Ifs[0])
		for _, c := range c.Ifs[1:] {
			ccond.Add(jen.Op("&&"))
			ccond.Add(s.goExpr(c))
		}
		cond = jen.If(ccond)
		iter.Block(cond)
	}

	return iter, cond
}

// print k=v either for function definitions (def=true) or for function call (def=false)
func (s *Scope) goKvals(kk []*ast.Keyword, def bool) *jen.Statement {
	return jen.ListFunc(func(g *jen.Group) {
		if def {
			for _, k := range kk {
				g.Add(goId(k.Arg).Commentf("/*=%v*/", s.goExpr(k.Value).GoString()))
			}
		} else {
			for _, k := range kk {
				g.Commentf("/*%v=*/", string(k.Arg)).Add(s.goExpr(k.Value))
			}
		}
	})
}

func (s *Scope) goExpr(expr interface{}) *jen.Statement {
	if verbose {
		log.Printf("XXX %T %#v\n\n", expr, expr)
	}

	switch v := expr.(type) {
	case []ast.Expr:
		return s.goExprList(v)

	case []*ast.Keyword:
		return jen.ListFunc(func(g *jen.Group) {
			for _, k := range v {
				g.Add(goId(k.Arg).Commentf("/*=%v*/", s.goExpr(k.Value).GoString()))
			}
		})

	case *ast.Tuple:
		return s.goInitialized(goTuple, v.Elts)

	case *ast.List:
		return s.goInitialized(goList, v.Elts)

	case *ast.Dict:
		return jen.Parens(goDict.Clone().Values(jen.DictFunc(func(d jen.Dict) {
			for i, k := range v.Keys {
				d[s.goExpr(k)] = s.goExpr(v.Values[i])
			}
		})))

	case *ast.Num:
		switch n := v.N.(type) {
		case py.Int:
			return jen.Lit(int(n))

		case py.Float:
			return jen.Lit(float64(n))

		case py.Complex:
			return jen.Lit(complex128(n))

		default:
			log.Printf("number %v at %v:%v", v.N, v.GetLineno(), v.GetColOffset())
			panic("invalid number")
		}

	case ast.Identifier:
		return goId(v)

	case *ast.NameConstant:
		switch v.Value {
		case py.None:
			return jen.Nil()

		case py.True:
			return jen.True()

		case py.False:
			return jen.False()

		default:
			s, _ := py.Str(v.Value)
			return jen.Lit(string(s.(py.String)))
		}

	case *ast.Str:
		return jen.Lit(string(v.S))

	case *ast.UnaryOp:
		if v.Op == ast.Invert {
			return jen.Op("-").Parens(s.goExpr(v.Operand).Op("+").Lit(1))
		} else {
			return s.goUnary(v.Op).Add(s.goExpr(v.Operand))
		}

	case *ast.BoolOp:
		stmt := s.goExpr(v.Values[0])
		for _, x := range v.Values[1:] {
			stmt.Add(s.goBoolOp(v.Op))
			stmt.Add(s.goExpr(x))
		}
		return stmt

	case *ast.BinOp:
		if v.Op == ast.Modulo { // %
			if _, ok := v.Left.(*ast.Str); ok { // this is really a formatting operation
				printfunc := jen.Qual("fmt", "Sprintf")
				printfmt := s.goExpr(v.Left)
				params := s.goExpr(v.Right)
				if tuple, ok := v.Right.(*ast.Tuple); ok {
					params = s.goExprList(tuple.Elts)
				}
				return printfunc.Params(printfmt, params)
			}
		}

		if v.Op == ast.Pow { // **
			return jen.Qual("math", "Pow").Params(s.goExpr(v.Left), s.goExpr(v.Right))
		}

		return s.goExpr(v.Left).Add(s.goOp(v.Op)).Add(s.goExpr(v.Right))

	case *ast.Compare:
		stmt := jen.Null()

		left := s.goExpr(v.Left)
		right := (*jen.Statement)(nil)

		for i, op := range v.Ops {
			if right != nil {
				stmt.Op("&&")
				left = right.Clone()
			}

			right = s.goExpr(v.Comparators[i])

			if op == ast.In {
				stmt.Add(goContains.Clone().Call(right, left))
			} else if op == ast.NotIn {
				stmt.Op("!").Add(goContains.Clone().Call(right, left))
			} else {
				stmt.Add(left)
				stmt.Add(s.goCmpOp(op))
				stmt.Add(right)
			}
		}

		return stmt

	case *ast.Name:
		return goId(v.Id)

	case *ast.Attribute:
		x, b, a := strAttribute(v)
		a = rename(a)

		if x != nil {
			return s.goExpr(x).Dot(a)
		}

		switch {
		case b == "re" && a == "compile":
			return jen.Qual("regexp", "MustCompile")

		case b == "re" && a == "match":
			return jen.Qual("regexp", "MatchString")

		case b == "sys" && a == "argv":
			return jen.Qual("os", "Args")

		case b == "sys" && a == "stdin":
			return jen.Qual("os", "Stdin")

		case b == "sys" && a == "stdout":
			return jen.Qual("os", "Stdout")

		case b == "sys" && a == "stderr":
			return jen.Qual("os", "Stderr")

		case b == "sys.stdin":
			return jen.Qual("os", "Stdin").Dot(a)

		case b == "sys.stdout":
			return jen.Qual("os", "Stdout").Dot(a)

		case b == "sys.stderr":
			return jen.Qual("os", "Stderr").Dot(a)
		}

		if imp, ok := s.imports[b]; ok {
			return jen.Qual(imp, a)
		}

		return jen.Id(b).Dot(a)

	case *ast.Subscript:
		return s.goSlice(v.Value, v.Slice)

	case *ast.Call:
		return s.goCall(v)

	case *ast.Lambda:
		args, _ := s.goFunctionArguments(v.Args, false)
		return jen.Func().Params(args).Block(s.goExpr(v.Body)).Call()

	case *ast.IfExp:
		return jen.Func().Params().Block(
			jen.If(s.goExpr(v.Test)).
				Block(jen.Return(s.goExpr(v.Body))).
				Else().
				Block(jen.Return(s.goExpr(v.Orelse)))).Call()

	case *ast.ListComp:
		outer, inner := s.gomprehension(v.Generators[0])
		for _, g := range v.Generators[1:] {
			outer1, inner1 := s.gomprehension(g)
			inner.Add(jen.Block(outer1))
			inner = inner1
		}
		inner.Add(jen.Block(jen.Id("lc").Op("=").Append(jen.Id("lc"), s.goExpr(v.Elt))))
		return jen.Func().Params().Params(jen.Id("lc").Add(goList)).Block(outer, jen.Return(jen.Id("lc"))).Call()

	case *ast.DictComp:
		outer, inner := s.gomprehension(v.Generators[0])
		for _, g := range v.Generators[1:] {
			outer1, inner1 := s.gomprehension(g)
			inner.Add(jen.Block(outer1))
			inner = inner1
		}
		inner.Add(jen.Block(jen.Id("mm").Index(s.goExpr(v.Key)).Op("=").Add(s.goExpr(v.Value))))
		return jen.Func().Params().Params(jen.Id("mm").Add(goDict)).Block(
			jen.Id("mm").Op("=").Add(goDict).Values(),
			outer,
			jen.Return()).Call()

	case *ast.GeneratorExp:
		outer, inner := s.gomprehension(v.Generators[0])
		for _, g := range v.Generators[1:] {
			outer1, inner1 := s.gomprehension(g)
			inner.Add(jen.Block(outer1))
			inner = inner1
		}
		inner.Add(jen.Block(jen.Id("c").Op("<-").Add(s.goExpr(v.Elt))))
		return jen.Func().Params().Params(jen.Id("c").Chan().Add(goAny)).Block(
			jen.Id("c").Op("=").Make(jen.Chan().Add(goAny)),
			jen.Go().Func().Params().Block(outer, jen.Close(jen.Id("c"))).Call(),
			jen.Return(),
		).Call()
	}

	return unknown("EXPR", expr)
}

func goId(id ast.Identifier) *jen.Statement {
	return jen.Id(rename(string(id)))
}

func (s *Scope) goFunctionArguments(args *ast.Arguments, skipReceiver bool) (*jen.Statement, *ast.Arg) {
	var recv *ast.Arg

	if args == nil {
		return jen.Null(), recv
	}

	var params []jen.Code

	aargs := args.Args
	if skipReceiver && len(aargs) > 0 {
		recv, aargs = aargs[0], aargs[1:]
	}

	for _, arg := range aargs {
		s.addName(arg.Arg)

		p := goId(arg.Arg)
		if arg.Annotation != nil {
			p.Add(s.goExpr(arg.Annotation))
		} else {
			p.Add(goAny)
		}

		params = append(params, p)
	}

	for i, arg := range args.Kwonlyargs {
		s.addName(arg.Arg)

		p := goId(arg.Arg)
		if arg.Annotation != nil {
			p.Add(s.goExpr(arg.Annotation))
		} else {
			p.Add(goAny)
		}

		p.Commentf("/*=%v*/", s.goExpr(args.KwDefaults[i]).GoString())
		params = append(params, p)
	}

	if args.Vararg != nil {
		s.addName(args.Vararg.Arg)

		p := goId(args.Vararg.Arg).Comment("/*...*/")
		if args.Vararg.Annotation != nil {
			p.Add(s.goExpr(args.Vararg.Annotation))
		} else {
			p.Add(goAny)
		}

		params = append(params, p)
	}

	if args.Kwarg != nil {
		s.addName(args.Kwarg.Arg)

		p := goId(args.Kwarg.Arg).Comment("/*...*/")
		if args.Vararg.Annotation != nil {
			p.Add(s.goExpr(args.Kwarg.Annotation))
		} else {
			p.Add(goAny)
		}

		params = append(params, p)
	}

	// XXX: what is arg.Defaults ?

	return jen.List(params...), recv
}

func strAttribute(attr *ast.Attribute) (ast.Expr, string, string) {
	var expr ast.Expr
	var base string

	switch v := attr.Value.(type) {
	case *ast.Attribute:
		_, b, a := strAttribute(v)
		base = b + "." + a
		expr = nil

	case *ast.Name:
		base = string(v.Id)
		expr = nil

	default:
		expr = attr.Value
	}

	return expr, base, string(attr.Attr)
}

func (s *Scope) goCallParams(params ...ast.Expr) *jen.Statement {
	return jen.ParamsFunc(func(g *jen.Group) {
		for _, p := range params {
			g.Add(s.goExpr(p))
		}
	})
}

func (s *Scope) goCall(call *ast.Call) *jen.Statement {
	cfunc := s.goExpr(call.Func)

	switch ff := call.Func.(type) {
	case *ast.Name:
		switch string(ff.Id) {
		case "print":
			cfunc = jen.Qual("fmt", "Println") // check for print parameters, could be fmt.Print, fmt.Fprint, etc.

		case "open":
			cfunc = jen.Qual("os", "Open") // could also be os.OpenFile

		case "isinstance": // isinstance(obj, type)
			if len(call.Args) == 2 {
				obj := s.goExpr(call.Args[0])
				otype := s.goExpr(call.Args[1])
				comment := jen.Commentf("isinstance(%v, %v)", obj.GoString(), otype.GoString())
				if attr, ok := call.Args[1].(*ast.Attribute); ok {
					otype = jen.Commentf("/*%v*/", s.goExpr(attr.Value).GoString()).Add(s.goExpr(attr.Attr))
				}
				return jen.Func().Params().Bool().Block(
					comment,
					jen.List(jen.Op("_"), jen.Id("ok")).Op(":=").Add(obj).Assert(otype),
					jen.Return(jen.Id("ok")),
				).Call()
			}

		case "type":
			cfunc = jen.Qual("reflect", "Type")
		}

	case *ast.Attribute:
		switch string(ff.Attr) {
		case "read":
			cfunc = s.goExpr(ff.Value).Dot("Read")

		case "write":
			cfunc = s.goExpr(ff.Value).Dot("Write")

		case "close":
			cfunc = s.goExpr(ff.Value).Dot("Close")

		case "items": // as in `for k, v in dict(a=1).items()`
			return s.goExpr(ff.Value) // remove items

		case "append":
			if len(call.Args) == 1 {
				return s.goExpr(ff.Value).Op("=").Id("append").
					Call(s.goExpr(ff.Value), s.goExpr(call.Args[0]))
			}

		case "upper":
			return jen.Qual("strings", "ToUpper").Call(s.goExpr(ff.Value))

		case "lower":
			return jen.Qual("strings", "ToLower").Call(s.goExpr(ff.Value))

		case "startswith":
			if len(call.Args) == 1 {
				return jen.Qual("strings", "HasPrefix").Call(s.goExpr(ff.Value), s.goExpr(call.Args[0]))
			}

		case "endswith":
			if len(call.Args) == 1 {
				return jen.Qual("strings", "HasSuffix").Call(s.goExpr(ff.Value), s.goExpr(call.Args[0]))
			}

		case "strip":
			if len(call.Args) == 0 {
				return jen.Qual("strings", "TrimSpace").Call(s.goExpr(ff.Value))
			} else if len(call.Args) == 1 {
				return jen.Qual("strings", "Trim").Call(s.goExpr(ff.Value), s.goExpr(call.Args[0]))
			}

		case "lstrip":
			if len(call.Args) == 0 {
				return jen.Qual(goRuntime, "TrimLeft").Call(s.goExpr(ff.Value))
			} else if len(call.Args) == 1 {
				return jen.Qual("strings", "TrimLeft").Call(s.goExpr(ff.Value), s.goExpr(call.Args[0]))
			}

		case "rstrip":
			if len(call.Args) == 0 {
				return jen.Qual(goRuntime, "TrimRight").Call(s.goExpr(ff.Value))
			} else if len(call.Args) == 1 {
				return jen.Qual("strings", "TrimRight").Call(s.goExpr(ff.Value), s.goExpr(call.Args[0]))
			}

		case "split":
			if len(call.Args) == 0 {
				return jen.Qual(goRuntime, "Splits").Call(s.goExpr(ff.Value))
			} else if len(call.Args) == 1 {
				return jen.Qual("strings", "Split").Call(s.goExpr(ff.Value), s.goExpr(call.Args[0]))
			} else if len(call.Args) == 2 {
				return jen.Qual("strings", "SplitN").Call(s.goExpr(ff.Value),
					s.goExpr(call.Args[0]),
					s.goExpr(call.Args[1]).Op("+").Lit(1))
			}

		case "join":
			if len(call.Args) == 1 {
				return jen.Qual("strings", "Join").Call(s.goExpr(call.Args[0]), s.goExpr(ff.Value))
			}

		case "replace":
			if len(call.Args) == 2 {
				return jen.Qual("strings", "Replace").Call(s.goExpr(ff.Value),
					s.goExpr(call.Args[0]),
					s.goExpr(call.Args[1]),
					jen.Lit(-1))
			} else if len(call.Args) == 3 {
				return jen.Qual("strings", "Replace").Call(s.goExpr(ff.Value),
					s.goExpr(call.Args[0]),
					s.goExpr(call.Args[1]),
					s.goExpr(call.Args[2]))
			}

		case "count":
			if len(call.Args) == 1 {
				return jen.Qual("strings", "Count").Call(s.goExpr(ff.Value), s.goExpr(call.Args[0]))
			}

		case "isspace":
			if len(call.Args) == 0 {
				return jen.Qual(goRuntime, "IsSpace").Call(s.goExpr(ff.Value))
			}

		case "isalpha":
			if len(call.Args) == 0 {
				return jen.Qual(goRuntime, "IsAlpha").Call(s.goExpr(ff.Value))
			}

		case "isdigit", "isnumeric":
			if len(call.Args) == 0 {
				return jen.Qual(goRuntime, "IsDigit").Call(s.goExpr(ff.Value))
			}

		case "isupper":
			if len(call.Args) == 0 {
				return jen.Qual(goRuntime, "IsUpper").Call(s.goExpr(ff.Value))
			}

		case "islower":
			if len(call.Args) == 0 {
				return jen.Qual(goRuntime, "IsLower").Call(s.goExpr(ff.Value))
			}

		case "reverse":
			if len(call.Args) == 0 {
				return jen.Qual(goRuntime, "Reverse").Call(s.goExpr(ff.Value))
			}
		}

		if name, ok := ff.Value.(*ast.Name); ok {
			switch {
			case string(name.Id) == "sys" && string(ff.Attr) == "exit":
				ret := jen.Lit(-1)
				if len(call.Args) > 0 {
					ret = s.goExpr(call.Args[0])
				}
				return jen.Qual("os", "Exit").Call(ret)

			case string(name.Id) == "time" && string(ff.Attr) == "sleep" && len(call.Args) == 1:
				tt := jen.Qual("time", "Duration").Parens(
					s.goExpr(call.Args[0]).Op("*").Float64().Parens(jen.Qual("time", "Second")))
				return jen.Qual("time", "Sleep").Call(tt)

			case string(name.Id) == "time" && string(ff.Attr) == "time" && len(call.Args) == 0:
				return jen.Qual("time", "Now").Call()
			}
		}
	}

	var args []jen.Code

	for _, arg := range call.Args {
		args = append(args, s.goExpr(arg))
	}

	if len(call.Keywords) > 0 {
		args = append(args, s.goKvals(call.Keywords, false))
	}

	if call.Starargs != nil {
		args = append(args, s.goExpr(call.Starargs).Comment("/*...*/"))
	}

	if call.Kwargs != nil {
		args = append(args, s.goExpr(call.Kwargs).Comment("/*...*/"))
	}

	return cfunc.Call(args...)
}

func (s *Scope) goFor(target, iter ast.Expr) (*jen.Statement, []ast.Expr) {
	for _, id := range exprIds(target) {
		s.addName(id)
	}

	if c, ok := iter.(*ast.Call); ok { // check for "for x in range(n)"
		//
		// for x in range(y)
		//
		if n, ok := c.Func.(*ast.Name); ok && string(n.Id) == "range" {
			if len(c.Args) < 1 || len(c.Args) > 3 {
				log.Printf("at %v:%v", iter.GetLineno(), iter.GetColOffset())
				panic("range expects 1 to 3 arguments")
			}

			start := jen.Lit(0)
			step := jen.Lit(1)

			var stop jen.Code

			if len(c.Args) == 1 {
				stop = s.goExpr(c.Args[0])
			} else {
				start = s.goExpr(c.Args[0])
				stop = s.goExpr(c.Args[1])

				if len(c.Args) > 2 {
					step = s.goExpr(c.Args[2])
				}
			}

			t := s.goExpr(target)

			return jen.For(t.Clone().Op(":=").Add(start),
				t.Clone().Op("<").Add(stop),
				t.Clone().Op("+=").Add(step)), nil
		}

		//
		// for i, v in enumerate(l)
		//
		if n, ok := c.Func.(*ast.Name); ok && string(n.Id) == "enumerate" && len(c.Args) == 1 {
			return jen.For(s.goExprOrList(target).Op(":=").Range().Add(s.goExpr(c.Args[0]))), nil
		}

		//
		// for v in iterator
		//
		//if lenExpr(target) <= 1 {
		//    return jen.For(jen.List(jen.Op("_"), s.goExpr(target)).Op(":=").Range().Add(s.goExpr(iter))), nil
		//}
	}

	// for x in iterable
	// for k, v in dict
	// for a,b,c in tuple iterable

	switch lenExpr(target) {
	case 0:
		log.Fatalf("for without target: %#v", target)

	case 1:
		return jen.For(jen.List(jen.Op("_"), s.goExpr(target)).Op(":=").Range().Add(s.goExpr(iter))), nil

	case 2:
		return jen.For(s.goExprOrList(target).Op(":=").Range().Add(s.goExpr(iter))), nil

	default:
		t := target.(*ast.Tuple)
		return jen.For(jen.Id("_t").Commentf("/* %s */", s.strExprList(t.Elts)).Op(":=").Range().Add(s.goExpr(iter))), t.Elts
	}

	return nil, nil // shouldn't get here
}

func (s *Scope) goAssign(assign *ast.Assign) (*jen.Statement, *jen.Statement, *jen.Statement) {
	goType := goAny.Clone()

	switch t := assign.Value.(type) {
	case *ast.Tuple:
		goType = goTuple.Clone()

	case *ast.List:
		goType = goList.Clone()

	case *ast.Dict:
		goType = goDict.Clone()

	case *ast.Str:
		goType = jen.String()

	case *ast.Num:
		switch t.N.(type) {
		case py.Int:
			goType = jen.Int()

		case py.Float:
			goType = jen.Float64()

		case py.Complex:
			goType = jen.Complex128()
		}
	}

	if len(assign.Targets) == 1 && (isTuple(assign.Targets[0]) || isList(assign.Targets[0])) {
		return s.goExprOrList(assign.Targets[0]), s.goExprOrList(assign.Value), goType
	}

	return s.goExpr(assign.Targets), s.goExpr(assign.Value), goType
}

// parse a block/list of statements anre returns
// - the block, as single statement
// - the list of statements (useful only in the main module)
// - the type of return (in a function body, true: yield, false: return)
func (s *Scope) parseBody(classname string, body []ast.Stmt) *jen.Statement {
	if verbose {
		log.Println("PARSE", s.level)
	}

	for i, stmt := range body {
		if i > 0 {
			s.Add(jen.Line())
		}

		if lineno {
			s.Add(jen.Commentf("// line %v\n", stmt.GetLineno()))
		}

		if expr, ok := stmt.(*ast.ExprStmt); ok {
			if str, ok := expr.Value.(*ast.Str); ok {
				// a top level string expression is a __doc__ string
				s.Add(jen.Comment(trimlines(str.S)).Line())
				continue
			}
		}

		switch v := stmt.(type) {
		case *ast.ImportFrom:
			s.imports[string(v.Module)] = string(v.Module)
			for _, i := range v.Names {
				if i.AsName != "" {
					s.Add(jen.Commentf("import %v %q // %v", i.AsName, v.Module, i.Name).Line())
				} else {
					s.Add(jen.Commentf("import %q // %v", v.Module, i.Name).Line())
				}
			}

		case *ast.Import:
			for _, i := range v.Names {
				if i.AsName != "" {
					s.Add(jen.Commentf("import %s %q", i.AsName, i.Name).Line())
					s.imports[string(i.AsName)] = string(i.Name)
				} else {
					s.Add(jen.Commentf("import %q", i.Name).Line())
					s.imports[string(i.Name)] = string(i.Name)
				}
			}

		case *ast.FunctionDef:
			var receiver jen.Code
			var returns jen.Code

			for _, d := range v.DecoratorList {
				s.Add(jen.Commentf("// @%v\n", s.goExpr(d).GoString()))
			}

			ss := s.Push()

			arguments, recv := ss.goFunctionArguments(v.Args, classname != "")
			if recv != nil {
				receiver = jen.Params(goId(recv.Arg).Op("*").Id(classname))
			}
			if v.Returns != nil && !isNone(v.Returns) {
				returns = jen.Params(ss.goExprOrList(v.Returns))
			}

			stmt := jen.Func()
			if receiver != nil {
				if string(v.Name) == "__str__" {
					stmt.Add(receiver).Id("String")
					returns = jen.Params(jen.Id("string"))
				} else {
					stmt.Add(receiver).Add(goId(v.Name))
				}
			} else if s.level < 1 {
				stmt.Add(goId(v.Name))
			} else {
				stmt = goId(v.Name).Op(":=").Func()
			}

			ss.returnType = ReturnNone
			parsed := ss.parseBody("", v.Body)
			if returns == nil && ss.returnType != ReturnNone {
				returns = goAny
			}

			ss.Pop(true)

			stmt.Params(arguments)
			if returns != nil {
				stmt.Add(returns)
			}

			stmt.Block(parsed).Line()
			s.Add(stmt)

		case *ast.ClassDef:
			//
                        // Here we should be expecting only:
                        //
                        // - pass (and nothing else)
                        // - string: should be a __doc__ string
                        // - assignments (class variable)
                        // - function definition (class methods)
                        // Anything else should be an error.
                        // So, we could convert:
                        // - pass: empty struct (done)
                        // - string: add comment to struct body
                        // - assignements: struct fields (with value in comment)
                        // - class methods: parse body and add to most outer scope
                        //
                        // NOTE that Python also allow class definitions inside a class definition
                        // (and probably more)
                        //

			ss := s.Push()

			classdef := jen.Type().Add(goId(v.Name)).StructFunc(func(g *jen.Group) {
				cdefs := ""

				if len(v.Bases) > 0 {
					cdefs += " " + s.strExprList(v.Bases)
				}

				if len(v.Keywords) > 0 {
					cdefs += " " + s.goExpr(v.Keywords).GoString()
				}

				if cdefs != "" {
					g.Add(jen.Commentf("%v", cdefs))
				}

				for _, pst := range v.Body {
					switch pv := pst.(type) {
					case *ast.Pass:
						continue

					case *ast.ExprStmt: // error if not string
						if str, ok := pv.Value.(*ast.Str); ok {
							g.Add(jen.Comment(string(str.S)))
						} else {
							log.Fatalf("unexpected expression in class definition: %#v", pv)
						}

					case *ast.Assign:
						target, value, typ := s.goAssign(pv)
						g.Add(target.Add(typ).Commentf("= %#v", value))

					case *ast.FunctionDef:
						s.methods = append(s.methods,
							ss.parseBody(string(v.Name), []ast.Stmt{pv}))

					default:
						log.Fatalf("unexpected statement in class definition: %#v", pv)
					}
				}
			}).Line()

			for _, d := range v.DecoratorList {
				s.Add(jen.Commentf("@%v\n", s.goExpr(d).GoString()))
			}

			s.Add(classdef)
			ss.Pop(true) // after s.Add(classdef), to add the methods after the type definition

		case *ast.Assign:
			target, value, _ := s.goAssign(v)
			stmt := target.Op("=").Add(value)
			if s.newNames(v.Targets) {
				stmt = jen.Var().Add(stmt)
			}
			s.Add(stmt)

		case *ast.AugAssign:
			s.Add(s.goExpr(v.Target).Add(s.goOpExt(v.Op, "=")).Add(s.goExpr(v.Value)))

		case *ast.ExprStmt:
			switch xStmt := v.Value.(type) {
			case *ast.Yield:
				ret := jen.Null()
				if xStmt.Value != nil {
					ret = s.goExprOrList(xStmt.Value)
				}
				//s.Add(jen.Commentf("yield %s", ret.GoString()))
				s.Add(jen.Return(ret).Comment("yield"))
				s.returnType = ReturnYield

			case *ast.YieldFrom:
				ret := jen.Null()
				if xStmt.Value != nil {
					ret = s.goExprOrList(xStmt.Value)
				}
				//s.Add(jen.Commentf("yield from %s", ret.GoString()))
				s.Add(jen.Return(ret).Comment("yield from"))
				s.returnType = ReturnYield

			default:
				s.Add(s.goExpr(v.Value)) //.Line()
			}

		case *ast.Pass:
			s.Add(jen.Comment("pass"))

		case *ast.Break:
			s.Add(jen.Break())

		case *ast.Continue:
			s.Add(jen.Continue())

		case *ast.Return:
			if v.Value == nil {
				s.Add(jen.Return())
			} else {
				s.Add(jen.Return(s.goExprOrList(v.Value)))
			}
			s.returnType = ReturnReturn

		case *ast.If:
			ss := s.Push()
			stmt := jen.If(s.goExpr(v.Test))
			if s.Top() && isNameMain(v.Test) && len(v.Orelse) == 0 {
				stmt = jen.Func().Id("main").Params()
				s.main = true
			}
			stmt.Block(ss.parseBody("", v.Body))
			if len(v.Orelse) > 0 {
				if _, ok := v.Orelse[0].(*ast.If); ok && len(v.Orelse) == 1 {
					stmt.Else().Add(ss.parseBody("", v.Orelse))
				} else {
					stmt.Else().Block(ss.parseBody("", v.Orelse))
				}
			}
			ss.Pop(false)
			s.Add(stmt)

		case *ast.For:
			ss := s.Push()
			stmt, targets := ss.goFor(v.Target, v.Iter)
			assgn := jen.Null()
			if targets != nil {
				assgn = ss.goExprList(targets).Op(":=").ListFunc(func(g *jen.Group) {
					for i := range targets {
						g.Add(jen.Id("_t").Index(jen.Lit(i)))
					}
				})
			}
			stmt.Block(assgn, ss.parseBody("", v.Body))
			if len(v.Orelse) > 0 {
				stmt.Else().Block(ss.parseBody("", v.Orelse))
			}
			ss.Pop(false)
			s.Add(stmt)

		case *ast.While:
			ss := s.Push()
			stmt := jen.For(ss.goExpr(v.Test))
			if k, ok := v.Test.(*ast.NameConstant); ok && k.Value == py.True {
				stmt = jen.For()
			}
			stmt = stmt.Block(ss.parseBody("", v.Body))
			if len(v.Orelse) > 0 {
				stmt.Else().Block(ss.parseBody("", v.Orelse))
			}
			ss.Pop(false)
			s.Add(stmt)

		case *ast.Try:
			ss := s.Push()
			stmt := jen.If(
				jen.Err().Op(":=").Func().Params().Params(goException).Block(
					jen.Comment("try"),
					ss.parseBody("", v.Body),
				).Call(),
				jen.Err().Op("!=").Nil())

			body := jen.Null()

			if len(v.Handlers) > 0 {
				body = jen.Switch(jen.Err()).BlockFunc(func(g *jen.Group) {
					g.Add(jen.Comment("except"))

					for _, h := range v.Handlers {
						ch := jen.Case(ss.goExpr(h.ExprType))
						if h.Name != "" {
							ch.Block(jen.Commentf("as %v", h.Name), ss.parseBody("", h.Body))
						} else {
							ch.Block(ss.parseBody("", h.Body))
						}

						g.Add(ch)
					}
				})
			}

			stmt.Block(body)

			if len(v.Orelse) > 0 {
				stmt.Else().Block(ss.parseBody("", v.Orelse))
			}

			if len(v.Finalbody) > 0 {
				stmt.Line().Block(jen.Comment("finally"), ss.parseBody("", v.Finalbody))
			}
			ss.Pop(false)
			s.Add(stmt)

		case *ast.Raise:
			stmt := jen.Return(goRaisedException.Call(s.goExpr(v.Exc)))
			if v.Cause != nil {
				stmt.Commentf("cause: %v", s.goExpr(v.Cause).GoString())
			}
			s.Add(stmt)

		case *ast.Assert:
			if v.Msg != nil {
				s.Add(goAssert.Call(s.goExpr(v.Test), s.goExpr(v.Msg)))
			} else {
				s.Add(goAssert.Call(s.goExpr(v.Test), jen.Lit("")))
			}

		case *ast.Global:
			s.Add(jen.Commentf("global %v", s.strIdentifiers(v.Names)))

		case *ast.Nonlocal:
			s.Add(jen.Commentf("nonlocal %v", s.strIdentifiers(v.Names)))

		case *ast.Delete:
			for _, t := range v.Targets {
				if st, ok := t.(*ast.Subscript); ok {
					if i, ok := st.Slice.(*ast.Index); ok {
						s.Add(jen.Delete(s.goExpr(st.Value), s.goExpr(i.Value)))
					} else {
						log.Panicf("unexpected DELETE %#v", st)
					}
				} else {
					s.Add(jen.Comment(unknown("DELETE", t).GoString()))
				}
			}

		case *ast.With:
			// We should really create an anonymous function
			// with a defer (that we can't really fill, but in a few cases)
			s.Add(jen.BlockFunc(func(g *jen.Group) {
				ss := s.Push()
				g.Comment("with")

				for _, item := range v.Items {
					if item.OptionalVars != nil {
						g.Add(ss.goExpr(item.OptionalVars).Op(":=").Add(ss.goExpr(item.ContextExpr)))
					} else {
						g.Add(ss.goExpr(item.ContextExpr))
					}
				}

				g.Line().Add(ss.parseBody("", v.Body))
				ss.Pop(false)
			}))

		default:
			s.Add(jen.Comment(unknown("STMT", stmt).GoString()))
		}
	}

	if verbose {
		log.Println("RETURN", s.returnType.String())
	}

	return s.Render()
}

func main() {
	flag.IntVar(&debugLevel, "d", debugLevel, "Parser debug level 0-4")
	flag.BoolVar(&panicUnknown, "panic", panicUnknown, "panic on unknown expression, to get a stacktrace")
	flag.BoolVar(&verbose, "verbose", verbose, "print statement and expressions")
	flag.BoolVar(&lineno, "lines", lineno, "add source line numbers")

	ignore := flag.Bool("ignore", false, "ignore errors")
	flag.Parse()

	parser.SetDebug(debugLevel)

	if len(flag.Args()) == 0 {
		log.Printf("Need files to parse")
		os.Exit(1)
	}

	for _, path := range flag.Args() {
		in, err := os.Open(path)
		if err != nil {
			log.Fatal(err)
		}

		defer in.Close()
		if debugLevel > 0 {
			log.Printf(path, "-----------------\n")
		}

		fi, err := in.Stat()
		if err != nil {
			log.Fatal(err)
		}

		tree, err := parser.Parse(in, path, "exec")
		if err != nil {
			log.Fatal(err)
		}

		m, ok := tree.(*ast.Module)
		if !ok {
			log.Fatal("expected Module, got", tree)
		}

		pname := strings.TrimSuffix(fi.Name(), ".py")
		f := jen.NewFile(pname)

		scope := NewScope(f)
		//scope.file.ImportAlias(goRuntime, ".")
		scope.parseBody("", m.Body)

		if scope.main {
			pname = "main"
		}

		fmt.Println("// generated by pygor")
		fmt.Println("package", pname)
		fmt.Println()
		scope.file.RenderImports(os.Stdout)

		stmts := append(scope.body, jen.Line())
		scope.file.ImportAlias(goRuntime, ".")

		for _, s := range stmts {
			if err := s.Render(os.Stdout); err != nil {
				if *ignore {
					fmt.Println("ERROR:", err)
				} else {
					log.Fatal(err)
				}
			}
		}
	}
}

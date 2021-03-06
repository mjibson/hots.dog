package main

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"

	"github.com/davecgh/go-spew/spew"
	"github.com/pkg/errors"
)

func main() {
	if err := extract(); err != nil {
		log.Fatalf("extract error: %+v", err)
	}
	fmt.Println("done: success")
}

var cleanRE = regexp.MustCompile(`<.*?>`)

func clean(s string) string {
	return strings.Join(cleanRE.Split(s, -1), "")
}

func extract() error {
	const dir = "mods"
	names := make(map[string]string)
	texts := make(map[string]string)
	tooltips := make(map[string]string)
	var x XML
	stringsWalk := func(path string, info os.FileInfo, err error) error {
		switch strings.ToLower(info.Name()) {
		case "gamestrings.txt":
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			scanner := bufio.NewScanner(f)
			for scanner.Scan() {
				line := scanner.Text()
				parts := strings.SplitN(line, "=", 2)
				const (
					heroname      = "Hero/Name/"
					buttonname    = "Button/Name/"
					buttontooltip = "Button/Tooltip/"
					simpletext    = "Button/SimpleDisplayText/"
				)
				if strings.HasPrefix(parts[0], heroname) {
					heroid := strings.TrimPrefix(parts[0], heroname)
					names[heroid] = parts[1]
				} else if strings.HasPrefix(parts[0], buttonname) {
					button := strings.TrimPrefix(parts[0], buttonname)
					names[button] = parts[1]
				} else if strings.HasPrefix(parts[0], simpletext) {
					text := strings.TrimPrefix(parts[0], simpletext)
					texts[text] = clean(parts[1])
				} else if strings.HasPrefix(parts[0], buttontooltip) {
					text := strings.TrimPrefix(parts[0], buttontooltip)
					tooltips[text] = parts[1]
					t := texts[text]
					// TODO: This should probably len(t) < len(parts[1]), but until the bribery
					// stacks bugs are fixed it's ok.

					if t == "" || len(t) > len(parts[1]) {
						texts[text] = clean(parts[1])
					}
				}
			}
			return scanner.Err()
		default:
			for _, s := range []string{
				"ActorData",
				"AnnouncerPackData",
				"EmoticonData",
				"LightData",
				"LootBox",
				"Mounts",
				"SkinData",
				"SoundData",
				"VOData",
				"VoiceOverData",
			} {
				if strings.Contains(strings.ToLower(path), strings.ToLower(s)) {
					return nil
				}
			}
			if strings.HasSuffix(path, ".xml") && (strings.HasPrefix(path, "mods/heromods/") ||
				strings.HasPrefix(path, "mods/heroesdata.stormmod/") ||
				strings.HasPrefix(path, "mods/core.stormmod/")) {
				fmt.Fprintln(os.Stderr, "LOADING", path)
				return x.loadXML(path)
			}
			return nil
		}
	}
	if err := filepath.Walk(dir, stringsWalk); err != nil {
		return errors.Wrap(err, "strings walk")
	}
	fmt.Fprintln(os.Stderr, "LOAD WALK DONE")

	type Talent struct {
		Name   string
		Desc   string
		Tier   int
		Column int
	}
	var wg sync.WaitGroup
	lock := make(chan bool, runtime.NumCPU())
	iconClean := func(s string) string {
		icon := strings.Replace(s, `\`, string(filepath.Separator), -1)
		parts := strings.Split(icon, string(filepath.Separator))
		parts[len(parts)-1] = strings.ToLower(parts[len(parts)-1])
		return filepath.Join(parts...)
	}
	makeTalentIcon := func(input, output string, args ...string) {
		input = filepath.Join("mods/heroes.stormmod/base.stormassets", strings.ToLower(input))
		output = filepath.Join("..", "frontend", "public", "img", output)
		wg.Add(1)
		go func() {
			lock <- true
			defer func() { <-lock; wg.Done() }()
			if _, err := os.Stat(input); err != nil {
				fmt.Println(input)
				panic(err)
			}
			if _, err := os.Stat(output); err == nil {
				// Already generated.
				// return
			}
			cargs := []string{input, "-strip", "-background", "black"}
			cargs = append(cargs, args...)
			cargs = append(cargs, output)
			if out, err := exec.Command("convert", cargs...).CombinedOutput(); err != nil {
				panic(errors.Errorf("%v: %s", err, out))
			}
			if out, err := exec.Command("optipng", output).CombinedOutput(); err != nil {
				panic(errors.Errorf("%v: %s", err, out))
			}
		}()
	}
	heroTalents := make(map[string][]*HeroTalent)
	icons := make(map[string]string)
	talentFaces := make(map[string]string)
	type Hero struct {
		Name      string
		ID        string
		Slug      string
		Role      string
		MultiRole []string
	}
	isMn := func(r rune) bool {
		return unicode.Is(unicode.Mn, r) // Mn: nonspacing marks
	}
	transformText := transform.Chain(norm.NFD, transform.RemoveFunc(isMn), norm.NFC)
	lettersRE := regexp.MustCompile(`[A-Za-z0-9]+`)
	cleanText := func(s string) string {
		b, err := ioutil.ReadAll(transform.NewReader(strings.NewReader(s), transformText))
		if err != nil {
			panic(err)
		}
		s = string(b)
		matches := lettersRE.FindAllStringSubmatch(s, -1)
		var buf bytes.Buffer
		for _, m := range matches {
			buf.WriteString(m[0])
		}
		s = buf.String()
		s = strings.ToLower(s)
		return s
	}
	var heroes []Hero
	// Not sure what is going on here, but this fixes it.
	faceMap := map[string]string{
		"ZeratulMightOfTheNerazimPassive": "ZeratulMightOfTheNerazimTalent",
	}
	walk := func(path string, _ os.FileInfo, err error) error {
		if !strings.HasSuffix(path, ".xml") {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return errors.Wrapf(err, "open %s", path)
		}
		defer f.Close()
		var v Catalog
		dec := xml.NewDecoder(f)
		dec.CharsetReader = func(charset string, r io.Reader) (io.Reader, error) {
			return r, nil
		}
		if err := dec.Decode(&v); err != nil {
			log.Printf("decode: %s: %v", path, err)
			return nil
			//return errors.Wrapf(err, "decode: %s", path)
		}
		for _, b := range v.CButton {
			icons[b.Id] = iconClean(b.Icon.Value)
		}
		for _, b := range v.CTalent {
			if v, ok := faceMap[b.Face.Value]; ok {
				talentFaces[b.Id] = v
			} else {
				talentFaces[b.Id] = b.Face.Value
			}
		}
		for _, chero := range v.CHero {
			if len(chero.TalentTreeArray) > 0 && chero.Id != "" {
				heroTalents[chero.Id] = chero.TalentTreeArray
			}
			if chero.Id != "" && len(chero.RolesMultiClass) != 0 {
				h := Hero{
					Name: names[chero.Id],
					ID:   chero.Id,
					Slug: cleanText(names[chero.Id]),
					Role: chero.CollectionCategory.Value,
				}
				if h.Name == "" {
					spew.Dump("H", h)
					spew.Dump("VCHERO", v.CHero)
					spew.Dump("CHERO", chero)
					spew.Dump("NAMES", names)
					panic(chero.Id)
				}
				{
					img := chero.Id
					if v := chero.ScoreScreenImage.Value; v != "" {
						img = iconClean(v)
					} else {
						img = iconClean(fmt.Sprintf(`assets\textures\storm_ui_ingame_hero_leaderboard_%s.dds`, img))
					}
					makeTalentIcon(img, filepath.Join("hero", h.Slug+".png"),
						"-resize", "40x40^", "-gravity", "center", "-extent", "40x40",
					)
					makeTalentIcon(img, filepath.Join("hero_full", h.Slug+".png"), "-resize", "100x56")
				}
				/*
					{
						img := chero.Id
						if v := chero.ScoreScreenImage.Value; v != "" {
							img = iconClean(v)
						} else {
							img = iconClean(fmt.Sprintf(`assets\textures\storm_ui_ingame_hero_loadingscreen_%s.dds`, img))
						}
						makeTalentIcon(img, filepath.Join("hero_loading", h.Slug+".png"))
					}
				*/
				for _, r := range chero.RolesMultiClass {
					h.MultiRole = append(h.MultiRole, r.Value)
				}
				heroes = append(heroes, h)
			}
		}
		return nil
	}
	if err := filepath.Walk(dir, walk); err != nil {
		return errors.Wrap(err, "xml walk")
	}

	/*
		enc := json.NewEncoder(os.Stderr)
		enc.SetIndent("", "\t")
		fmt.Println("ICONS")
		enc.Encode(icons)
		fmt.Println("FACES")
		enc.Encode(talentFaces)
		fmt.Println("NAMES")
		enc.Encode(names)
	*/

	sort.Slice(heroes, func(i, j int) bool {
		return heroes[i].Name < heroes[j].Name
	})

	// Verify we have data for all current talents.
	heroTalentLookup := map[string][7][]string{}
	for hero, talents := range heroTalents {
		var t [7][]string
		for _, talent := range talents {
			tier := t[talent.Tier-1]
			if talent.Column != len(tier)+1 {
				panic(talent)
			}
			t[talent.Tier-1] = append(tier, talent.Talent)
			face := talentFaces[talent.Talent]
			if face == "" {
				panic(talent.Talent)
			}
			if names[face] == "" {
				panic(talent.Talent)
			}
			if texts[face] == "" {
				panic(fmt.Errorf("%s: %s", talent.Talent, face))
			}
		}
		heroTalentLookup[hero] = t
	}

	var keys []string
	for k := range talentFaces {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Populate full tooltips.
	{
		var wg sync.WaitGroup
		var mlock sync.Mutex
		for _, k := range keys {
			v := talentFaces[k]
			mlock.Lock()
			t := texts[v]
			mlock.Unlock()
			if t == "" {
				continue
			}
			if tooltip := tooltips[v]; tooltip != "" {
				wg.Add(1)
				go func(k, v, tooltip string) {
					lock <- true
					defer func() { <-lock; wg.Done() }()
					//start := time.Now()
					tip, err := getTooltip(tooltip, x)
					//fmt.Fprintf(os.Stderr, "getTooltip: %s (%s)\n", k, time.Since(start))
					if tip != "" && err == nil {
						mlock.Lock()
						texts[v] = tip
						mlock.Unlock()
					} else {
						fmt.Fprintf(os.Stderr, "notooltip: %s: %v\n", v, err)
					}
				}(k, v, tooltip)
			}
		}
		wg.Wait()
	}

	out := new(bytes.Buffer)

	fmt.Fprint(out, `package main

type Hero struct {
	Name      string
	ID        string
	Slug      string
	Role      string
	MultiRole []string
	Talents   [7][]string
}

var heroData = []Hero{`)

	for _, h := range heroes {
		fmt.Fprintf(out, `
	{
		Name:      %q,
		ID:        %q,
		Slug:      %q,
		Role:      %q,
		MultiRole: %#v,
		Talents:   %#v,
	},`, h.Name, h.ID, h.Slug, h.Role, h.MultiRole, heroTalentLookup[h.ID])
	}

	fmt.Fprint(out, `
}

type talentText struct {
	Name string
	Text string
}

var talentData = map[string]talentText{`)

	for _, k := range keys {
		v := talentFaces[k]
		t := texts[v]
		n := names[v]
		icon := icons[v]
		if t == "" || n == "" || icon == "" {
			continue
		}
		makeTalentIcon(icon, filepath.Join("talent", k+".png"), "-resize", "40x40!")
		fmt.Fprintf(out, `
	%q: {
		Name: %q,
		Text: %q,
	},`, k, n, t)
	}
	fmt.Fprint(out, `
}
`)
	wg.Wait()
	if err := ioutil.WriteFile("../talents.go", out.Bytes(), 0666); err != nil {
		return err
	}
	return exec.Command("gofmt", "-w", "-s", "../talents.go").Run()
}

var (
	reC   = regexp.MustCompile(`(?i:</?[scki].*?>)`)
	reN   = regexp.MustCompile(`(</?n/?>)+`)
	reD1  = regexp.MustCompile(`\[d.*?/\]`)
	reD2  = regexp.MustCompile(`<d.*?/>`)
	reVal = regexp.MustCompile(`[A-Z][_A-Za-z0-9,\[\].]+`)
)

func getTooltip(s string, x XML) (string, error) {
	gotErr := false
	lookup := func(s string) string {
		v, err := x.Get(s)
		if err != nil {
			gotErr = true
			fmt.Fprintf(os.Stderr, "UNKNOWN1: %v (%q) s\n", err, s)
			return "UNKNOWN1"
		}
		if v == "" {
			gotErr = true
			fmt.Fprintf(os.Stderr, "not found: %s\n", s)
			return "0"
		}
		return v
	}
	s = reC.ReplaceAllString(s, "")
	s = reN.ReplaceAllString(s, "\n")
	// Don't truncate during [d ref] section.
	fFmt := "%f"
	dRepl := func(r string) string {
		if r[0] == '[' {
			r = fmt.Sprintf("<%s>", r[1:len(r)-1])
		}
		t, err := xml.NewDecoder(strings.NewReader(r)).Token()
		if err != nil {
			panic(err)
		}
		se := t.(xml.StartElement)
		var v string
		fmtStr := fFmt
		for _, attr := range se.Attr {
			switch strings.ToLower(attr.Name.Local) {
			case "ref":
				v = reVal.ReplaceAllStringFunc(attr.Value, lookup)
			case "precision":
				fmtStr = fmt.Sprintf("%%0.%sf", attr.Value)
			}
		}
		if v == "" {
			gotErr = true
			fmt.Fprintf(os.Stderr, "UNKNOWN3: %s: %s\n", s, r)
			return "UNKNOWN3"
		}
		if gotErr {
			return v
		}
		f := evalExpr(v)
		v = fmt.Sprintf(fmtStr, f)
		if strings.Contains(v, ".") {
			v = strings.TrimRight(v, "0")
			if strings.HasSuffix(v, ".") {
				v = v[:len(v)-1]
			}
		}
		return v
	}
	s = reD1.ReplaceAllStringFunc(s, dRepl)
	fFmt = "%.0f"
	s = reD2.ReplaceAllStringFunc(s, dRepl)
	var err error
	if gotErr {
		err = errors.New("error")
	}
	return s, err
}

const jsonTemplate = `package main

type TalentData

var talentData map[string]TalentData
`

type Catalog struct {
	CCharacter struct {
		Id string `xml:"id,attr"`
	}
	CTalent []struct {
		Id   string `xml:"id,attr"`
		Face Value
	}
	CButton []struct {
		Id   string `xml:"id,attr"`
		Icon Value
	}
	CHero []struct {
		Id                 string `xml:"id,attr"`
		TalentTreeArray    []*HeroTalent
		CollectionCategory Value
		RolesMultiClass    []struct {
			Value string `xml:"value,attr"`
		}
		ScoreScreenImage Value
	}
}

type Value struct {
	Value string `xml:"value,attr"`
}

type HeroTalent struct {
	Talent string `xml:"Talent,attr"`
	Tier   int    `xml:"Tier,attr"`
	Column int    `xml:"Column,attr"`
}

type TalentText struct {
	Name string
	Text string
}

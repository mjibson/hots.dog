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

	"github.com/pkg/errors"
)

func main() {
	if err := extract(); err != nil {
		log.Fatalf("%+v", err)
	}
}

var cleanRE = regexp.MustCompile(`<.*?>`)

func clean(s string) string {
	return strings.Join(cleanRE.Split(s, -1), "")
}

func extract() error {
	const dir = "mods"
	names := make(map[string]string)
	texts := make(map[string]string)
	stringsWalk := func(path string, info os.FileInfo, err error) error {
		if info.Name() != "GameStrings.txt" {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		scanner := bufio.NewScanner(f)
		defer f.Close()
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
				t := texts[text]
				if t == "" || len(t) > len(parts[1]) {
					texts[text] = clean(parts[1])
				}
			}
		}
		return scanner.Err()
	}
	if err := filepath.Walk(dir, stringsWalk); err != nil {
		return err
	}

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
		input = filepath.Join("mods/heroes.stormmod/base.stormassets", input)
		output = filepath.Join("..", "frontend", "public", "img", output)
		wg.Add(1)
		go func() {
			lock <- true
			defer func() { <-lock; wg.Done() }()
			if _, err := os.Stat(input); err != nil {
				panic(input)
			}
			if _, err := os.Stat(output); err == nil {
				// Already generated.
				return
			}
			cargs := []string{input}
			cargs = append(cargs, args...)
			cargs = append(cargs, output)
			if out, err := exec.Command("convert", cargs...).CombinedOutput(); err != nil {
				panic(errors.Errorf("%v: %s", err, out))
			}
		}()
	}
	heroTalents := make(map[string][]*HeroTalent)
	icons := make(map[string]string)
	talentFaces := make(map[string]string)
	type Hero struct {
		Name      string
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
	walk := func(path string, _ os.FileInfo, err error) error {
		if !strings.HasSuffix(path, ".xml") {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		var v Catalog
		dec := xml.NewDecoder(f)
		dec.CharsetReader = func(charset string, r io.Reader) (io.Reader, error) {
			return r, nil
		}
		if err := dec.Decode(&v); err != nil {
			return err
		}
		for _, b := range v.CButton {
			icons[b.Id] = iconClean(b.Icon.Value)
		}
		for _, b := range v.CTalent {
			talentFaces[b.Id] = b.Face.Value
		}
		for _, chero := range v.CHero {
			if len(chero.TalentTreeArray) > 0 && chero.Id != "" {
				heroTalents[chero.Id] = chero.TalentTreeArray
			}
			if chero.Id != "" && len(chero.RolesMultiClass) != 0 {
				h := Hero{
					Name: names[chero.Id],
					Slug: cleanText(names[chero.Id]),
					Role: chero.CollectionCategory.Value,
				}
				if h.Name == "" {
					panic(chero.Id)
				}
				{
					img := chero.Id
					if v := chero.ScoreScreenImage.Value; v != "" {
						img = iconClean(v)
					} else {
						img = iconClean(fmt.Sprintf(`Assets\Textures\storm_ui_ingame_hero_leaderboard_%s.dds`, img))
					}
					makeTalentIcon(img, filepath.Join("hero", h.Slug+".png"),
						"-resize", "40x40^", "-gravity", "center", "-extent", "40x40",
					)
					makeTalentIcon(img, filepath.Join("hero_full", h.Slug+".png"), "-resize", "100x56")
				}
				{
					img := chero.Id
					if v := chero.ScoreScreenImage.Value; v != "" {
						img = iconClean(v)
					} else {
						img = iconClean(fmt.Sprintf(`Assets\Textures\storm_ui_ingame_hero_loadingscreen_%s.dds`, img))
					}
					makeTalentIcon(img, filepath.Join("hero_loading", h.Slug+".png"))
				}
				for _, r := range chero.RolesMultiClass {
					h.MultiRole = append(h.MultiRole, r.Value)
				}
				heroes = append(heroes, h)
			}
		}
		return nil
	}
	if err := filepath.Walk(dir, walk); err != nil {
		return err
	}
	/*
		fmt.Println("ICONS")
		enc.Encode(icons)
		fmt.Println("FACES")
		enc.Encode(talentFaces)
		fmt.Println("NAMES")
		enc.Encode(names)
		fmt.Println("TEXTS")
		enc.Encode(texts)
		//*/

	sort.Slice(heroes, func(i, j int) bool {
		return heroes[i].Name < heroes[j].Name
	})

	// Verify we have data for all current talents.
	for _, talents := range heroTalents {
		for _, talent := range talents {
			face := talentFaces[talent.Talent]
			if face == "" {
				panic(talent.Talent)
			}
			if names[face] == "" {
				panic(talent.Talent)
			}
			if texts[face] == "" {
				panic(talent.Talent)
			}
		}
	}

	fmt.Print(`package main

type Hero struct {
	Name      string
	Slug      string
	Role      string
	MultiRole []string
}

var heroData = []Hero{`)

	for _, h := range heroes {
		fmt.Printf(`
	{
		Name:      %q,
		Slug:      %q,
		Role:      %q,
		MultiRole: %#v,
	},`, h.Name, h.Slug, h.Role, h.MultiRole)
	}

	fmt.Print(`
}

type talentText struct {
	Name string
	Text string
}

var talentData = map[string]talentText{`)

	var keys []string
	for k := range talentFaces {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		v := talentFaces[k]
		t := texts[v]
		n := names[v]
		icon := icons[v]
		if t == "" || n == "" || icon == "" {
			continue
		}
		makeTalentIcon(icon, filepath.Join("talent", k+".png"), "-resize", "40x40")
		fmt.Printf(`
	%q: {
		Name: %q,
		Text: %q,
	},`, k, n, t)
	}
	fmt.Print(`
}
`)
	wg.Wait()
	return nil
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
		Face struct {
			Value string `xml:"value,attr"`
		}
	}
	CButton []struct {
		Id   string `xml:"id,attr"`
		Icon struct {
			Value string `xml:"value,attr"`
		}
	}
	CHero []struct {
		Id                 string `xml:"id,attr"`
		TalentTreeArray    []*HeroTalent
		CollectionCategory struct {
			Value string `xml:"value,attr"`
		}
		RolesMultiClass []struct {
			Value string `xml:"value,attr"`
		}
		ScoreScreenImage struct {
			Value string `xml:"value,attr"`
		}
	}
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
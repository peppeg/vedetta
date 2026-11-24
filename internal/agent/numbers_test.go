package agent

import (
	"reflect"
	"testing"
)

// V25-QA A1.0: the table of docs/SPEC_QA_PACCHETTO_ONESTA.md §2.
func TestMissingClaimNumbers(t *testing.T) {
	cases := []struct {
		claim, evidence string
		missing         []string
		question        string
	}{
		// separators and decimals
		{"1.000 operai", "1,000 workers", nil, ""},
		{"1000 workers", "1 000 ouvriers", nil, ""},
		{"1’000 Arbeiter", "1000", nil, ""},
		{"12,5%", "12.5 %", nil, ""},
		{"2.000.000 di abitanti", "2,000,000 inhabitants", nil, ""},
		{"costa 12.500,50 euro", "12500.50", nil, ""},
		{"3.50 dollars", "3,5 $", nil, ""},
		// ranges and dates, piece by piece
		{"costruito tra 1859 e 1869", "1859-1869", nil, ""},
		{"completato nel 1869", "inaugurato il 17 novembre 1869", nil, ""},
		{"inaugurato il 25 novembre 1869", "17 novembre 1869", []string{"25"}, ""},
		// numbers in words, 5 languages
		{"three months", "3 mesi", nil, ""},
		{"dieci studi", "ten RCTs", nil, ""},
		{"zwei Dokumente", "2 Unterlagen", nil, ""},
		{"vingt-trois pays", "23 countries", nil, ""},
		{"treinta y dos casos", "32", nil, ""},
		{"ventitré anni", "23 years", nil, ""},
		{"dreiundzwanzig", "23", nil, ""},
		{"veintitrés", "23", nil, ""},
		// multipliers
		{"44 mila abitanti", "44.000", nil, ""},
		{"44.000 abitanti", "44 mila", nil, ""},
		{"2 million people", "2.000.000", nil, ""},
		{"1,5 miliardi", "1.5 billion", nil, ""},
		{"3 Millionen", "3 million", nil, ""},
		{"mille operai", "1000", nil, ""},
		{"duemila", "2000", nil, ""},
		{"trecento metri", "300", nil, ""},
		{"a thousand workers", "mille operai", nil, ""},
		// decades and centuries
		{"late 1980s", "negli anni '80", nil, ""},
		{"anni '80", "the 1980s", nil, ""},
		{"anni ottanta", "1980s", nil, ""},
		{"années 80", "the eighties", nil, ""},
		{"80er Jahre", "1980s", nil, ""},
		{"XIX secolo", "19th century", nil, ""},
		{"siglo XIX", "XIX secolo", nil, ""},
		{"XIXe siècle", "19. Jahrhundert", nil, ""},
		// real model additions (m1Hbj) must be caught
		{"Suez (1859-1869)", "the canal opened in 1869", []string{"1859"}, ""},
		{"2009-15", "between 2009 and 2012", []string{"2015"}, ""},
		{"altezza di 330 metri", "324 metri", []string{"330"}, ""},
		// rounding is not guessed (D3)
		{"12,5%", "12,47%", []string{"12.5"}, ""},
		// weak "one" is never required; names with numbers are numbers like the others
		{"one of the largest", "among the largest", nil, ""},
		{"Zen 5 core", "Zen 5", nil, ""},
		{"D.Lgs 199/2021", "decreto legislativo 8 novembre 2021, n. 199", nil, ""},
		{"PostgreSQL 18", "PostgreSQL 17", []string{"18"}, ""},
		// false alarms found in the offline test (A1.1, 21/10)
		{"proroga al 30/06/2026", "fino al 30 giugno 2026", nil, ""},
		{"entro il 31/12/2025", "entro il 31 dicembre 2025", nil, ""},
		{"RC1 released Oct 15", "the first release candidate arrived on October 15", nil, ""},
		{"tariffa monoraria F0 a 0,154 €/kWh", "0,154 €/kWh in tariffa monoraria", nil, ""},
		{"The Principle of Hope (1954–1959)", "(Bloch 1954–59 [1986a: 673])", nil, ""},
		{"The Eiffel Tower grew by 6 meters due to the addition of a digital antenna", "grew by 6 metres", nil, ""},
		{"tre mesi di attesa", "3 Monate", nil, ""},
		{"una troupe di 36.000 operai", "trentaseimila operai, tecnici, ingegneri", nil, ""},
		{"36000 Arbeiter", "sechsunddreißigtausend Arbeiter", nil, ""},
		// numbers of the user's question may be repeated by the claim
		{"Valencia 3-day forecast: max 26°C", "13 / 26", nil, "Che tempo farà a Valencia nei prossimi 3 giorni?"},
		{"Valencia 3-day forecast: max 26°C", "13 / 26", []string{"3"}, ""},
		// scales (case 19, 05/11/2026)
		{"3 trillion dollars", "3.000 miliardi di dollari", nil, ""},
		{"3 trilioni di dollari", "3 trillion dollars", nil, ""},
		{"mille miliardi", "1 trillion", nil, ""},
		{"cento milioni", "100 million", nil, ""},
		{"2 milioni di miliardi", "2 million billion", nil, ""},
		{"2 milioni di miliardi", "2 milioni", []string{"2000000000000000"}, ""},
		{"10 mila miliardi", "10 trillion", nil, ""},
		{"50mila abitanti", "50.000", nil, ""},
		{"4,35 milioni", "4.350.000", nil, ""},
		// a word in the plural is a range, not a number
		{"migliaia di miliardi di volte", "thousands of trillions of times", []string{"migliaia di miliardi"}, ""},
		{"milioni di miliardi di volte", "thousands of trillions of times", nil, ""},
		{"migliaia di miliardi di dollari", "5 trillion dollars", nil, ""},
		{"centinaia di milioni di collisioni", "hundreds of millions of collisions", nil, ""},
		{"centinaia di milioni di collisioni", "300 milioni", nil, ""},
		{"centinaia di milioni di collisioni", "30 milioni", []string{"centinaia di milioni"}, ""},
		{"decine di migliaia", "tens of thousands", nil, ""},
		{"el proyecto costó miles de millones de euros para el país", "it cost 3 billion euros", nil, ""},
		{"milioni di utenti", "300 milioni di utenti", nil, ""},
		{"miliardi di dollari", "3 milioni di dollari", []string{"miliardi"}, ""},
		{"2.000 operai", "migliaia di operai", []string{"2000"}, ""},
		// alone they fit almost anything: on the evidence side only
		{"thousands of workers", "migliaia di operai", nil, ""},
		{"migliaia di operai", "molti operai", nil, ""},
		{"decine di persone", "poche persone", nil, ""},
		// other meanings
		{"the road is 3 miles long", "3 miles", nil, ""},
		{"30 milioni. Mille persone", "30 million. 1000 people", nil, ""},
		// hertz
		{"40 milioni di incroci al secondo", "letti a 40 MHz", nil, ""},
		{"40 milioni di incroci al secondo", "letti a 40MHz", nil, ""},
		{"2,4 GHz", "2400 MHz", nil, ""},
		{"5 GHz", "2.4 GHz", []string{"5"}, ""},
		// no numbers at all
		{"il canale fu costruito da operai egiziani", "", nil, ""},
	}
	for _, c := range cases {
		if got := MissingClaimNumbers(c.claim, c.evidence, c.question); !reflect.DeepEqual(got, c.missing) {
			t.Errorf("claim %q / evidence %q: missing %v, want %v", c.claim, c.evidence, got, c.missing)
		}
	}
}

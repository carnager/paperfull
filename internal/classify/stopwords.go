package classify

import "strings"

// Common German and English words that say nothing about what a document is.
var stopwords = func() map[string]struct{} {
	m := map[string]struct{}{}
	for _, w := range strings.Fields(`
aber alle allem allen aller alles als also ander andere anderem anderen anderer anderes auch auf aus bei beim bin bis bist bitte
damit dann das dass dein deine dem den denn der des dessen deshalb die dies diese diesem diesen dieser dieses dich dir doch dort
durch ein eine einem einen einer eines einige einigen einiger einiges etwa etwas euch euer eure für gegen gewesen habe haben hat
hatte hatten hier hin hinter ihm ihn ihnen ihr ihre ihrem ihren ihrer ihres indem ins ist jede jedem jeden jeder jedes jene jetzt
kann kein keine keinen können könnte machen man manche mehr mein meine mich mir mit muss musste nach nicht nichts noch nun nur
oder ohne sehr sein seine seinem seinen seiner selbst sich sie sind solche soll sollte sondern sonst über und uns unser unsere
unter viel vom von vor war waren warst was weg weil weiter welche welchem welchen welcher welches wenn werde werden wie wieder
will wir wird wirst wollen wollte würde würden zum zur zwar zwischen sowie bzw ggf inkl sehr geehrte geehrter damen herren
freundlichen grüßen grüssen mfg
the and for are but not you all any can had her was one our out has have this that with from they will would there their
what about which when your into than them then these some been were also its may per please dear who how why all each
other such only own same too very just should could does did doing being here where while because over under again further
once more most both few nor off yours ours hers his him she yourself thank thanks regards sincerely
`) {
		m[w] = struct{}{}
	}
	return m
}()

func isStopword(w string) bool {
	_, ok := stopwords[w]
	return ok
}

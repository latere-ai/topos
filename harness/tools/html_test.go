// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import "testing"

func TestHTMLText(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"empty", "", ""},
		{"only markup", "<div><span></span></div>", ""},
		{"plain", "just   text\n here", "just text here\n"},
		{"entities", "<p>a &lt;b&gt; &amp; &quot;c&quot; &#169; &nbsp;d</p>", "a <b> & \"c\" © d\n"},
		{"paragraphs", "<p>one</p><p>two</p>", "one\n\ntwo\n"},
		{"lines", "a<br>b<br/>c<div>d</div>e", "a\nb\nc\nd\ne\n"},
		{"headings", "<h1>Title</h1><h3>Sub</h3>text", "# Title\n\n### Sub\n\ntext\n"},
		{"list", "<ul><li>one</li><li>two <b>bold</b></li></ul>after", "- one\n- two bold\n\nafter\n"},
		{"table", "<table><tr><th>a</th><th>b</th></tr><tr><td>1</td><td>2</td></tr></table>", "a b\n1 2\n"},
		{"inline", "a<b>b</b><i> c </i>d", "ab c d\n"},
		{"dropped", "a<script type=\"x\">if (a < b) { x() }</script>b<style>p{}</style>c<svg><text>t</text></svg>d<noscript>n</noscript>e<template>t</template>f", "abcdef\n"},
		{"dropped upper case", "a<SCRIPT>x</SCRIPT>b", "ab\n"},
		{"unclosed script", "a<script>never closed", "a\n"},
		{"comment", "a<!-- hidden <p> -->b", "ab\n"},
		{"unclosed comment", "a<!-- never closed", "a\n"},
		{"doctype", "<!DOCTYPE html><?xml version=\"1.0\"?>a", "a\n"},
		{"unclosed doctype", "a<!DOCTYPE", "a\n"},
		{"stray lt", "1 < 2 and 3 <4", "1 < 2 and 3 <4\n"},
		{"lt at end", "a <", "a <\n"},
		{"quoted gt", "<a href=\"x>y\" title='>'>link</a>", "link\n"},
		{"tag without end", "a<div class=\"x\"", "a\n"},
		{"pre", "<p>x</p><pre>  keep\n    this  </pre>y", "x\n\n  keep\n    this\n\ny\n"},
		{"nested pre", "<pre><pre>a  b</pre>  c  </pre>  d  e", "a  b\n\n  c\n\nd e\n"},
		{"stray pre close", "</pre>a  b", "a b\n"},
		{"attributes with numbers", "<h2 id=\"x\">A</h2><x-el:y>b</x-el:y>", "## A\n\nb\n"},
		{"invalid utf-8", "a\xffb", "a\uFFFDb\n"},
		{"invalid utf-8 in pre", "<pre>a\xffb</pre>", "a\uFFFDb\n"},
		{"space before a marker", "a <li>b</li>", "a\n- b\n"},
		{"space after text before pre", "a <pre>b</pre>", "a\n\nb\n"},
		{"pre only newlines", "<pre>\n\n</pre>x", "x\n"},
		{"cell after text", "x<td>y</td>", "x y\n"},
	} {
		if got := HTMLText([]byte(c.in)); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

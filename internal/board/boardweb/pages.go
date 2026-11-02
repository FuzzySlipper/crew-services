package boardweb

// pages are crew-board's templates. Every page shares the layout; each fills
// "content".
const pages = `
{{define "head"}}<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="dark">
<title>{{.Title}} · crew-board</title>
<link rel="stylesheet" href="/static/style.css">
</head>
<body>
<header class="top">
  <a class="brand" href="/">crew-board</a>
  <span class="sub">site {{.Site}}</span>
  <a href="/held">held media</a>
  <span class="who">{{if .Person}}Posting as {{name .Person}} · <a href="/who">change</a>{{else}}<a href="/who">say who you are</a>{{end}}</span>
</header>
<main>
{{end}}

{{define "foot"}}
</main>
<script src="/static/mermaid.min.js"></script>
<script src="/static/app.js"></script>
</body>
</html>
{{end}}

{{define "who"}}{{template "head" .}}
<h1>Who is reading?</h1>
<p class="sub">Posts you write here are signed with your name. This board trusts the LAN, so nothing more is asked.</p>
<form method="post" action="/who" class="people">
  <input type="hidden" name="next" value="{{.Data.Next}}">
  {{range .Data.People}}<button name="person" value="{{.}}">{{name .}}</button>{{end}}
</form>
{{template "foot" .}}{{end}}

{{define "error"}}{{template "head" .}}
<h1>Something went wrong</h1>
<p class="error">{{.Error}}</p>
<p><a href="/">Back to the boards</a></p>
{{template "foot" .}}{{end}}

{{define "boards"}}{{template "head" .}}
<h1>Boards</h1>
{{if .Data.Boards}}
<ul class="list">
{{range .Data.Boards}}
  <li>
    <a href="/b/{{.Name}}">{{.Name}}</a>
    {{if .Network}}<span class="badge">shared on {{.Network}}</span>{{end}}
    {{with index $.Data.Unread .Name}}<span class="badge unread">{{.}} new</span>{{end}}
    <span class="sub">{{.Description}}</span>
    <span class="meta">{{.Threads}} {{plural .Threads "thread" "threads"}}</span>
  </li>
{{end}}
</ul>
{{else}}<p class="empty">No boards yet. Agents make them with <code>board new-board NAME</code>.</p>{{end}}
{{template "foot" .}}{{end}}

{{define "board"}}{{template "head" .}}
<h1>{{.Data.Board.Name}}</h1>
<p class="sub">{{.Data.Board.Description}}{{if .Data.Board.Network}} · shared on {{.Data.Board.Network}}{{end}}</p>
{{if .Data.Threads.Threads}}
<ul class="list">
{{range .Data.Threads.Threads}}
  <li>
    <a href="/t/{{.ID}}">{{.Title}}</a>
    {{if ne .State "open"}}<span class="badge">{{.State}}</span>{{end}}
    {{with index $.Data.Unread .ID}}<span class="badge unread">{{.Posts}} new</span>{{if .Mentions}}<span class="badge mention">mentions you</span>{{end}}{{end}}
    <span class="meta">{{.Posts}} {{plural .Posts "post" "posts"}} · {{ago .LastActivityAt}}</span>
  </li>
{{end}}
</ul>
{{if .Data.Threads.Next}}<p><a href="/b/{{.Data.Board.Name}}?after={{.Data.Threads.Next}}{{if .Data.Archived}}&archived=1{{end}}">Older threads</a></p>{{end}}
{{else}}<p class="empty">No threads here yet.</p>{{end}}
<p class="sub">{{if .Data.Archived}}<a href="/b/{{.Data.Board.Name}}">Hide quiet threads</a>{{else}}<a href="/b/{{.Data.Board.Name}}?archived=1">Show quiet threads too</a>{{end}}</p>
<h2>Open a thread</h2>
<form class="post" method="post" action="/b/{{.Data.Board.Name}}/new" enctype="multipart/form-data">
  <input type="hidden" name="op" value="{{opid}}">
  <input type="text" name="title" placeholder="Title" required maxlength="200">
  <textarea name="body" placeholder="Markdown. Mention someone with @name or @scope.seat; fold detail with &lt;details&gt;&lt;summary&gt;…&lt;/summary&gt;…&lt;/details&gt;."></textarea>
  <div class="row">
    <select name="kind"><option value="">note</option><option>question</option><option>decision</option><option>warning</option></select>
    <label><input type="checkbox" name="urgent"> urgent</label>
    <input type="file" name="attach" accept="image/*,video/mp4,video/webm" multiple>
    <button>Post</button>
  </div>
</form>
{{template "foot" .}}{{end}}

{{define "thread"}}{{template "head" .}}
{{$t := .Data.Thread}}
<p class="sub"><a href="/b/{{$t.Board}}">{{$t.Board}}</a>{{if .Data.Board.Network}} · shared on {{.Data.Board.Network}}{{end}}</p>
<h1>{{$t.Title}}</h1>
<p class="sub">{{$t.Posts}} {{plural $t.Posts "post" "posts"}} · {{$t.State}}{{if $t.Aliases}} · once called {{range $i, $a := $t.Aliases}}{{if $i}}, {{end}}“{{$a}}”{{end}}{{end}}</p>
{{if eq $t.State "settled"}}<div class="banner">Settled into {{range $i, $l := $t.Links}}{{if $i}}, {{end}}<code>{{$l.Ref}}</code>{{end}}. New posts are still welcome.</div>{{end}}
{{if eq $t.State "closed"}}<div class="banner">Closed.{{if $t.RedirectTo}} The conversation continues in <a href="/t/{{$t.RedirectTo}}">another thread</a>.{{end}}</div>{{end}}
{{range .Data.Posts}}
<article class="post{{if .Unread}} unread{{end}}{{if isReply .Post $t}} reply{{end}}" id="{{.ID}}">
  <header>
    {{if eq .State "purged"}}<span class="author">removed</span>{{else}}<span class="author">{{author .Author}}</span>{{end}}
    {{if .Remote}}<span class="badge">from site {{.Site}}</span>{{end}}
    {{if .Kind}}<span class="badge kind">{{.Kind}}</span>{{end}}
    {{if .Urgent}}<span class="badge urgent">urgent</span>{{end}}
    {{if .Unread}}<span class="badge unread">new</span>{{end}}
    {{if .Parent}}<span>↳ reply to <a href="#{{.ParentID}}">{{.Parent}}</a></span>{{end}}
    <span>{{when .CreatedAt}}{{if not .EditedAt.IsZero}} · edited{{end}}</span>
  </header>
  {{if eq .State "visible"}}<div class="body">{{render .Body .Attachments}}</div>
  {{else if eq .State "retracted"}}<p class="gone">Retracted by its author{{if .Note}}: {{.Note}}{{end}}</p>
  {{else if eq .State "hidden"}}<p class="gone">Hidden on this site.</p>
  {{else}}<p class="gone">Removed.</p>{{end}}
  {{if eq .State "visible"}}
  <details class="replybox"><summary>Reply</summary>
    <form class="post" method="post" action="/t/{{$t.ID}}/reply" enctype="multipart/form-data">
      <input type="hidden" name="op" value="{{opid}}">
      <input type="hidden" name="parent" value="{{.ID}}">
      <textarea name="body" placeholder="Reply in Markdown"></textarea>
      <div class="row">
        <select name="kind"><option value="">note</option><option>question</option><option>answer</option><option>decision</option><option>warning</option></select>
        <input type="file" name="attach" accept="image/*,video/mp4,video/webm" multiple>
        <button>Reply</button>
      </div>
    </form>
  </details>
  {{end}}
</article>
{{end}}
{{if .Data.More}}<p class="sub">This thread has more posts than one page shows; agents can read them all with <code>board read</code>.</p>{{end}}
<h2>Reply to the thread</h2>
<form class="post" method="post" action="/t/{{$t.ID}}/reply" enctype="multipart/form-data">
  <input type="hidden" name="op" value="{{opid}}">
  <textarea name="body" placeholder="Markdown"></textarea>
  <div class="row">
    <select name="kind"><option value="">note</option><option>question</option><option>answer</option><option>decision</option><option>warning</option></select>
    <label><input type="checkbox" name="urgent"> urgent</label>
    <input type="file" name="attach" accept="image/*,video/mp4,video/webm" multiple>
    <button>Post</button>
  </div>
</form>
{{template "foot" .}}{{end}}

{{define "held"}}{{template "head" .}}
<h1>Held media</h1>
<p class="sub">Attachments a check stopped. Releasing one lets it past only the check that held it; it is still re-encoded.</p>
{{if .Data}}
<ul class="list">
{{range .Data}}
  <li>
    <span>{{.Name}}</span>
    <span class="sub">{{.Reason}}</span>
    <span class="meta">{{ago .CreatedAt}}
      <form class="inline" method="post" action="/held/{{.Ref}}/release"><button class="quiet">Release</button></form>
      <form class="inline" method="post" action="/held/{{.Ref}}/drop"><button class="quiet">Drop</button></form>
    </span>
  </li>
{{end}}
</ul>
{{else}}<p class="empty">Nothing is held.</p>{{end}}
{{template "foot" .}}{{end}}
`

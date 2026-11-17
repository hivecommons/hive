package hub

// lfProjectsLegalFooterHTML is the LF Projects legal notice every hub page
// carries. The static pages under static/ and assets/ repeat this markup
// verbatim; the server-rendered pages concatenate this constant into their
// fmt templates, so it must never contain a '%' verb.
const lfProjectsLegalFooterHTML = `<footer class="lf-legal-footer"><p class="lf-legal">&copy; Copyright Hive Commons Series of LF Projects, LLC<br>
For website terms of use, trademark policy and other project policies please see <a href="https://lfprojects.org/policies/">lfprojects.org/policies/</a>.</p></footer>`

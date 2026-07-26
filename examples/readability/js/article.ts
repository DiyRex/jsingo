/**
 * Article extraction, backed by Mozilla's Readability.
 *
 * This is the reason jsingo exists. Readability is the engine behind Firefox's
 * Reader View and has years of accumulated fixes for real-world markup. The Go
 * ports are community efforts that lag it. Rather than reimplement or accept a
 * stale port, the sidecar runs the upstream library directly.
 *
 * linkedom supplies the DOM: it is a fraction of jsdom's weight and Readability
 * only needs parsing and traversal, not layout or script execution. Not
 * executing scripts is also the point - the input is untrusted HTML.
 */

import { Readability, isProbablyReaderable } from "@mozilla/readability";
import { parseHTML } from "linkedom";

export interface ParseRequest {
  html: string;
  /** Absolute URL of the page, used to resolve relative links. */
  url?: string;
}

export interface ParseResponse {
  title: string;
  byline: string;
  excerpt: string;
  siteName: string;
  lang: string;
  content: string;
  textContent: string;
  length: number;
  readerable: boolean;
}

/** Thrown when the document has no extractable article. */
class NotFound extends Error {
  readonly code = 5; // wire.CodeNotFound
  constructor(message: string) {
    super(message);
    this.name = "NotFound";
  }
}

class InvalidArgument extends Error {
  readonly code = 3; // wire.CodeInvalidArgument
  constructor(message: string) {
    super(message);
    this.name = "InvalidArgument";
  }
}

function buildDocument(html: string, url?: string): Document {
  const { document } = parseHTML(html);

  // Readability resolves relative hrefs and image sources against the
  // document's base URI. Without this, extracted content from a real page
  // comes back with links that go nowhere.
  if (url) {
    const base = document.createElement("base");
    base.setAttribute("href", url);
    document.head?.appendChild(base);
  }
  return document as unknown as Document;
}

/** Extracts the main article from a page. */
export function parseArticle(req: ParseRequest): ParseResponse {
  if (!req?.html || req.html.trim() === "") {
    throw new InvalidArgument("html is required");
  }

  const document = buildDocument(req.html, req.url);
  const readerable = isProbablyReaderable(document);

  // Readability mutates the document it is given, so nothing may reuse it.
  const article = new Readability(document).parse();
  if (!article) {
    throw new NotFound("no extractable article content");
  }

  return {
    title: article.title ?? "",
    byline: article.byline ?? "",
    excerpt: article.excerpt ?? "",
    siteName: article.siteName ?? "",
    lang: article.lang ?? "",
    content: article.content ?? "",
    textContent: article.textContent ?? "",
    length: article.length ?? 0,
    readerable,
  };
}

/**
 * Reports whether a page looks like an article, without extracting it.
 *
 * Much cheaper than parseArticle: useful for filtering a crawl before paying
 * for full extraction.
 */
export function isReaderable(req: ParseRequest): { readerable: boolean } {
  if (!req?.html) throw new InvalidArgument("html is required");
  return { readerable: isProbablyReaderable(buildDocument(req.html, req.url)) };
}

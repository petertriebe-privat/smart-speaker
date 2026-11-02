#pragma once

// Tool web_search. With TAVILY_API_KEY (secrets.h) the device asks Tavily's Search API itself:
// a short answer plus sources, counted per calendar month in /quota.json ("tavily", like the
// Google APIs in services/maps). Without a key it is OpenAI's hosted web search, billed per call.
namespace websearch {

bool configured();
void addTools();

}  // namespace websearch

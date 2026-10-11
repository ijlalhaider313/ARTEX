# Filter Rules Guide

Documentation for the rules that filter Flows in the web UI.

## Basic filter example

```
github
```

Explanation: filters Flows whose URL contains `github`.

## A scope can be specified before the keyword

| Scope     | Description                             |
| --------- | --------------------------------------- |
| url       | Request URL                             |
| method    | HTTP request method (GET / POST, etc.)  |
| code      | HTTP response status code               |
| reqheader | Request headers                         |
| resheader | Response headers                        |
| header    | Request headers or response headers     |
| reqbody   | Request body                            |
| resbody   | Response body                           |
| body      | Request body or response body           |
| all       | Any of URL / Method / Header / Body     |

## Filter rule examples with a scope

### URL filter

```
url:github
```

Explanation: filters Flows whose URL contains `github`. The default scope is the URL.

### Request method filter

```
method:get
```

Explanation: filters GET requests. The match is case-insensitive.

### Response status code filter

```
code:404
```

Explanation: filters Flows whose response status code is 404.

### Header filter

```
header:application/json
```

Explanation: filters Flows whose request headers or response headers contain `application/json`.

### Request body / response body filter

```
reqbody:token
resbody:token
body:token
```

Explanation: filters Flows whose request body, response body, or request body/response body respectively contains `token`.

### Filter across everything

```
all:token
```

Explanation: filters Flows in which any of the URL / Header / Body contains `token`.

### Filters containing a space

```
resbody:"hello world"
```

Explanation: filters Flows whose response body contains `hello world`.

## Logical operators

### or

```
google or baidu
```

Explanation: filters Flows whose URL contains `google` or `baidu`.

### and

```
method:post and body:hello
```

Explanation: filters POST requests whose request body or response body contains `hello`.

### not

```
not url:github
```

Explanation: filters Flows whose URL does not contain `github`.

### Combining operators

```
method:get and (url:google or url:baidu) and not resheader:html
```

Explanation: filters GET requests whose URL contains `google` or `baidu` and whose response headers do not contain `html`.

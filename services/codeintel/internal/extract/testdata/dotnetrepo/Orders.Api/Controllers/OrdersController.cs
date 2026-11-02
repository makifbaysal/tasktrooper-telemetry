using Microsoft.AspNetCore.Mvc;
using Npgsql;

namespace Orders.Api.Controllers;

/// <summary>
/// [HttpGet("doc-comment")] is not a route.
/// </summary>
[ApiController]
[Route("api/[controller]")]
public sealed class OrdersController(NpgsqlDataSource db) : ControllerBase
{
    private const string Search = "search";

    [HttpGet]
    public async Task<IActionResult> List() => Ok(await db.OpenConnectionAsync());

    [HttpGet("{id:guid}", Name = "GetOrder")]
    public IActionResult Get(Guid id)
    {
        var note = "[HttpGet(\"in-string\")]";
        return Ok(new { id, note });
    }

    [HttpPost(Name = "CreateOrder")]
    [ProducesResponseType(201)]
    public IActionResult Create([FromBody] OrderDto dto)
    {
        return CreatedAtRoute("GetOrder", new { id = Guid.NewGuid() }, dto);
    }

    [HttpDelete("/admin/orders/{id}")]
    public IActionResult Purge(int id) => NoContent();

    [Route("[action]")]
    [HttpPut]
    public Task ArchiveAsync() => Task.CompletedTask;

    [AcceptVerbs("GET", "HEAD", Route = Search + "/{*term}")]
    public IActionResult Find(string term) => Ok(term);

    [HttpPatch("~/v1/orders/{id:int:min(1)}/status")]
    public IActionResult Status(int id) => Ok();

    // [HttpGet("commented")]
    // public IActionResult Commented() => Ok();

    /* [HttpGet("block-commented")] */
    public IActionResult NotAnAction() => Ok();

    [NonAction]
    [HttpGet("non-action")]
    public IActionResult Helper() => Ok();
}

public record OrderDto(string Sku, int Quantity);

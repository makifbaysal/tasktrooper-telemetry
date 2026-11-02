using Microsoft.AspNetCore.Mvc;

[ApiController]
[Route("fake")]
public class FakeController : ControllerBase
{
    [HttpGet("from-tests")]
    public IActionResult Get() => Ok();
}

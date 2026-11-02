using Microsoft.AspNetCore.Mvc;

namespace Orders.Api.Controllers
{
    [ApiController]
    [Route(Routes.V2 + "/[controller]")]
    public abstract class ApiControllerBase : ControllerBase
    {
    }

    public static class Routes
    {
        public const string V2 = "api/v2";
    }

    public class InvoicesController : ApiControllerBase
    {
        [HttpGet("{id:int}")]
        public IActionResult Get(int id) => Ok(id);

        [HttpPost("{id}/[action]")]
        public IActionResult Pay(int id)
        {
            return Ok();
        }
    }
}
